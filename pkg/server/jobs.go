package server

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// JobExecutor defines the execution contract for individual scheduled jobs.
type JobExecutor interface {
	ExecuteJob(ctx context.Context, job *Job) error
}

// ServerJobExecutor executes jobs using server components and GitHub/LLM clients.
type ServerJobExecutor struct {
	server *Server
	store  JobStore
}

func NewServerJobExecutor(s *Server, store JobStore) *ServerJobExecutor {
	return &ServerJobExecutor{
		server: s,
		store:  store,
	}
}

func (e *ServerJobExecutor) ExecuteJob(ctx context.Context, job *Job) error {
	switch job.Kind {
	case "review":
		return e.executeReviewJob(ctx, job)
	case "labels":
		return e.executeLabelsJob(ctx, job)
	case "describe":
		return e.executeDescribeJob(ctx, job)
	case "improve":
		return e.executeImproveJob(ctx, job)
	case "summary":
		return e.executeSummaryJob(ctx, job)
	case "changelog":
		return e.executeChangelogJob(ctx, job)
	case "docs":
		return e.executeDocsJob(ctx, job)
	case "assistant":
		return e.executeAssistantJob(ctx, job)
	default:
		job.Status = "failed"
		job.Error = fmt.Sprintf("unknown job kind: %s", job.Kind)
		return e.store.UpdateJob(ctx, job)
	}
}

func (e *ServerJobExecutor) executeReviewJob(ctx context.Context, job *Job) error {
	gh := e.server.gh
	engine := e.server.engine

	// 1. Visible queued status comment with marker
	statusMarker := fmt.Sprintf("<!-- pr-review-status:%s -->", job.ID)
	statusIntent := &OutputIntent{
		Marker:    statusMarker,
		JobID:     job.ID,
		Action:    "status",
		PRKey:     job.PRKey,
		Owner:     job.Owner,
		Repo:      job.Repo,
		PRNumber:  job.PRNumber,
		ExactHead: job.HeadSHA,
		Body:      "⏳ Review queued; waiting for capacity",
		Status:    "pending",
	}

	if err := e.store.SaveOutputIntent(ctx, statusIntent); err != nil {
		log.Printf("[jobs] Failed saving initial status intent for %s: %v", job.ID, err)
	}

	statusCommentID, err := gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, statusIntent.Body)
	if err == nil {
		statusIntent.CommentID = statusCommentID
		statusIntent.Status = "in_progress"
		_ = e.store.UpdateOutputIntent(ctx, statusIntent)

		job.StatusCommentID = statusCommentID
		_ = e.store.UpdateJob(ctx, job)

		// Transition status comment to running
		_ = gh.EditComment(ctx, job.Owner, job.Repo, statusCommentID, "🔄 Review running...")
	} else {
		log.Printf("[jobs] Warning: Failed creating status comment for %s: %v", job.ID, err)
	}

	// 2. Head-bound review execution
	report, err := engine.ReviewPRAtHead(ctx, job.Owner, job.Repo, job.PRNumber, job.HeadSHA)
	if err != nil {
		log.Printf("[jobs] Review failed for %s: %v", job.ID, err)
		if job.StatusCommentID > 0 {
			_ = gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, fmt.Sprintf("❌ Review failed: %v", err))
		}
		job.Status = "failed"
		job.Error = err.Error()
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return err
	}

	// 3. Verify live head immediately before posting to prevent stale review publication
	livePR, prErr := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr == nil && livePR.HeadSHA != job.HeadSHA {
		log.Printf("[jobs] Head moved from %s to %s for %s; discarding outdated report", job.HeadSHA, livePR.HeadSHA, job.ID)
		if job.StatusCommentID > 0 {
			_ = gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, "⏭️ Review superseded by newer commit.")
		}
		job.Status = "superseded"
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return nil
	}

	// 4. Save review output intent before posting
	reviewMarker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
	boundedMarkdown := report.RawMarkdown
	if len(boundedMarkdown) > MaxReviewBodyBytes {
		boundedMarkdown = boundedMarkdown[:MaxReviewBodyBytes]
	}

	reviewIntent := &OutputIntent{
		Marker:    reviewMarker,
		JobID:     job.ID,
		Action:    "review_output",
		PRKey:     job.PRKey,
		Owner:     job.Owner,
		Repo:      job.Repo,
		PRNumber:  job.PRNumber,
		ExactHead: job.HeadSHA,
		Body:      boundedMarkdown,
		Status:    "pending",
	}
	if err := e.store.SaveOutputIntent(ctx, reviewIntent); err != nil {
		log.Printf("[jobs] Failed saving review output intent for %s: %v", job.ID, err)
	}

	// 5. Post review output
	outputCommentID, err := gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, boundedMarkdown)
	if err != nil {
		log.Printf("[jobs] Failed posting review comment for %s: %v", job.ID, err)
		reviewIntent.Status = "uncertain"
		_ = e.store.UpdateOutputIntent(ctx, reviewIntent)
		job.Status = "uncertain"
		job.Error = err.Error()
		_ = e.store.UpdateJob(ctx, job)
		return err
	}

	reviewIntent.CommentID = outputCommentID
	reviewIntent.Status = "completed"
	_ = e.store.UpdateOutputIntent(ctx, reviewIntent)

	// 6. Update status comment to completed
	if job.StatusCommentID > 0 {
		_ = gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, "✅ Review completed.")
	}

	// 7. Update PR state last reviewed head and mark job completed
	prState, _ := e.store.GetPRState(ctx, job.PRKey)
	if prState != nil {
		prState.LastReviewedHead = job.HeadSHA
		_ = e.store.UpdatePRState(ctx, prState)
	}

	job.Status = "completed"
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return nil
}

func (e *ServerJobExecutor) executeLabelsJob(ctx context.Context, job *Job) error {
	if e.server.labeler == nil {
		return nil
	}
	_, err := e.server.labeler.RunAndApply(ctx, job.Owner, job.Repo, job.PRNumber)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

func (e *ServerJobExecutor) executeDescribeJob(ctx context.Context, job *Job) error {
	if e.server.describer == nil {
		return nil
	}
	_, err := e.server.describer.RunAndUpdate(ctx, job.Owner, job.Repo, job.PRNumber)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

func (e *ServerJobExecutor) executeImproveJob(ctx context.Context, job *Job) error {
	const message = "## PR Improvement Suggestions\n\nOne-click suggestions are unavailable until isolated project verification is configured. No build or tests were run."
	err := e.server.gh.PostComment(ctx, job.Owner, job.Repo, job.PRNumber, message)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

func (e *ServerJobExecutor) executeSummaryJob(ctx context.Context, job *Job) error {
	if e.server.summarizer == nil {
		return nil
	}
	_, err := e.server.summarizer.RunAndPost(ctx, job.Owner, job.Repo, job.PRNumber)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

func (e *ServerJobExecutor) executeChangelogJob(ctx context.Context, job *Job) error {
	if e.server.changelog == nil {
		return nil
	}
	err := e.server.changelog.RunAndPost(ctx, job.Owner, job.Repo, job.PRNumber)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

func (e *ServerJobExecutor) executeDocsJob(ctx context.Context, job *Job) error {
	// Reuses server's dispatchAddDocs behavior with context
	e.server.dispatchAddDocs(job.Owner, job.Repo, job.PRNumber)
	job.Status = "completed"
	now := time.Now().UTC()
	job.FinishedAt = &now
	return e.store.UpdateJob(ctx, job)
}

func (e *ServerJobExecutor) executeAssistantJob(ctx context.Context, job *Job) error {
	if e.server.assistant == nil {
		return nil
	}
	_, err := e.server.assistant.RunAndReply(ctx, job.Owner, job.Repo, job.PRNumber, job.Payload)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
	} else {
		job.Status = "completed"
	}
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return err
}

// Scheduler coordinates FIFO job execution with per-PR serialisation and fixed workers.
type Scheduler struct {
	store     JobStore
	executor  JobExecutor
	workers   int
	activePRs map[string]bool
	mu        sync.Mutex
	wakeCh    chan struct{}
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewScheduler(store JobStore, executor JobExecutor, workers int) *Scheduler {
	if workers <= 0 {
		workers = 2
	}
	return &Scheduler{
		store:     store,
		executor:  executor,
		workers:   workers,
		activePRs: make(map[string]bool),
		wakeCh:    make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
	}
}

func (s *Scheduler) Start(ctx context.Context) error {
	// Recover interrupted jobs on startup
	if _, err := s.store.RecoverInterruptedJobs(ctx); err != nil {
		log.Printf("[scheduler] Warning during job recovery: %v", err)
	}

	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.workerLoop(ctx, i)
	}

	// Trigger initial work check
	s.Wake()
	return nil
}

func (s *Scheduler) Stop() {
	close(s.stopCh)
	s.wg.Wait()
}

func (s *Scheduler) Wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) workerLoop(ctx context.Context, workerID int) {
	defer s.wg.Done()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		job := s.claimNextJob(ctx)
		if job == nil {
			select {
			case <-s.wakeCh:
			case <-time.After(100 * time.Millisecond):
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		_ = s.executor.ExecuteJob(ctx, job)
		s.releaseJob(job)
	}
}

func (s *Scheduler) claimNextJob(ctx context.Context) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	queuedJobs, err := s.store.ListQueuedJobs(ctx)
	if err != nil || len(queuedJobs) == 0 {
		return nil
	}

	// Find the oldest eligible job whose PRKey is not currently active
	var target *Job
	for _, j := range queuedJobs {
		prKeyStr := j.PRKey.String()
		if !s.activePRs[prKeyStr] {
			target = j
			s.activePRs[prKeyStr] = true
			break
		}
	}

	if target == nil {
		return nil
	}

	target.Status = "running"
	now := time.Now().UTC()
	target.StartedAt = &now
	_ = s.store.UpdateJob(ctx, target)
	return target
}

func (s *Scheduler) releaseJob(job *Job) {
	s.mu.Lock()
	delete(s.activePRs, job.PRKey.String())
	s.mu.Unlock()
	s.Wake()
}
