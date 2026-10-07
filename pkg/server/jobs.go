package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
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

	// 1. Initial head check before expensive generation
	livePR, prErr := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr != nil {
		log.Printf("[jobs] Failed fetching live PR for %s: %v", job.ID, prErr)
		job.Status = "failed"
		job.Error = prErr.Error()
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return prErr
	}

	if err := ghclient.ValidateCommitOID(livePR.HeadSHA); err != nil {
		job.Status = "failed"
		job.Error = fmt.Sprintf("invalid live head SHA %s: %v", livePR.HeadSHA, err)
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return fmt.Errorf("invalid live head SHA: %w", err)
	}

	if err := ghclient.ValidateCommitOID(livePR.BaseSHA); err != nil {
		job.Status = "failed"
		job.Error = fmt.Sprintf("invalid live base SHA %s: %v", livePR.BaseSHA, err)
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return fmt.Errorf("invalid live base SHA: %w", err)
	}

	prState, _ := e.store.GetPRState(ctx, job.PRKey)

	// A job-head mismatch before generation updates/requeues intent without expensive work.
	if livePR.HeadSHA != job.HeadSHA {
		log.Printf("[jobs] Head mismatch before generation for %s (job=%s, live=%s)", job.ID, job.HeadSHA, livePR.HeadSHA)
		if job.Trigger == "automatic" {
			if prState != nil && prState.LastReviewedHead == livePR.HeadSHA {
				// Current live head was already reviewed; supersede without expensive work
				job.Status = "superseded"
				now := time.Now().UTC()
				job.FinishedAt = &now
				_ = e.store.UpdateJob(ctx, job)
				return nil
			}
			job.HeadSHA = livePR.HeadSHA
			job.BaseSHA = livePR.BaseSHA
			_ = e.store.UpdateJob(ctx, job)
		} else {
			// Explicit review retargets same request
			job.HeadSHA = livePR.HeadSHA
			job.BaseSHA = livePR.BaseSHA
			_ = e.store.UpdateJob(ctx, job)
		}
	}

	// 2. Status comment: check if fresh rerun on already reviewed head
	isRerun := (job.Trigger == "explicit" && prState != nil && prState.LastReviewedHead == job.HeadSHA)
	var initialStatusBody string
	if isRerun {
		initialStatusBody = "This commit was already reviewed. Reviewing again."
	} else {
		initialStatusBody = "⏳ Review queued; waiting for capacity"
	}

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
		Body:      initialStatusBody,
		Status:    "pending",
	}

	if err := e.store.SaveOutputIntent(ctx, statusIntent); err != nil {
		log.Printf("[jobs] Failed saving initial status intent for %s: %v", job.ID, err)
	}

	statusCommentID, err := gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, initialStatusBody)
	if err == nil {
		statusIntent.CommentID = statusCommentID
		statusIntent.Status = "in_progress"
		_ = e.store.UpdateOutputIntent(ctx, statusIntent)

		job.StatusCommentID = statusCommentID
		_ = e.store.UpdateJob(ctx, job)

		if !isRerun {
			// Transition status comment to running
			_ = gh.EditComment(ctx, job.Owner, job.Repo, statusCommentID, "🔄 Review running...")
		}
	} else {
		log.Printf("[jobs] Warning: Failed creating status comment for %s: %v", job.ID, err)
	}

	// 3. Head-bound review execution
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

	// 4. Verify generation and live head immediately before publication to suppress stale results
	prState, _ = e.store.GetPRState(ctx, job.PRKey)
	generationChanged := prState != nil && prState.Generation > job.Generation

	livePR2, prErr2 := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	headChanged := prErr2 == nil && livePR2 != nil && livePR2.HeadSHA != job.HeadSHA

	if generationChanged || headChanged {
		log.Printf("[jobs] Generation or head changed during review of %s (genChanged=%v, headChanged=%v); discarding outdated report",
			job.ID, generationChanged, headChanged)
		if job.StatusCommentID > 0 {
			_ = gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, "⏭️ Review superseded by newer commit.")
		}

		if job.Trigger == "explicit" {
			// Explicit review retargets same request
			if livePR2 != nil {
				job.HeadSHA = livePR2.HeadSHA
				job.BaseSHA = livePR2.BaseSHA
			}
			job.Status = "queued"
			_ = e.store.UpdateJob(ctx, job)
		} else {
			job.Status = "superseded"
			now := time.Now().UTC()
			job.FinishedAt = &now
			_ = e.store.UpdateJob(ctx, job)

			// If head changed without a delivered event, schedule latest-head review intent using reserved successor
			if headChanged && livePR2 != nil {
				_, _ = e.store.ScheduleSuccessorReview(ctx, job.PRKey, job.Owner, job.Repo, job.PRNumber, livePR2.BaseSHA, livePR2.HeadSHA)
			}
		}
		return nil
	}

	// Disagreement between OID, diff, source, and report is a failure before publication
	if report.HeadSHA != job.HeadSHA || (livePR2 != nil && livePR2.HeadSHA != job.HeadSHA) {
		job.Status = "failed"
		job.Error = "OID/diff/source/report disagreement before publication"
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return errors.New("OID/diff/source/report disagreement before publication")
	}

	// 5. Save review output intent before posting
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

	// 6. Post review output
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

	// 7. Update status comment to completed
	if job.StatusCommentID > 0 {
		_ = gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, "✅ Review completed.")
	}

	// 8. Update PR state last reviewed head and mark job completed
	prState, _ = e.store.GetPRState(ctx, job.PRKey)
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

	target, err := s.store.ClaimNextJob(ctx, s.activePRs)
	if err != nil || target == nil {
		return nil
	}

	s.activePRs[target.PRKey.String()] = true
	return target
}

func (s *Scheduler) releaseJob(job *Job) {
	s.mu.Lock()
	delete(s.activePRs, job.PRKey.String())
	_ = s.store.ReleasePR(context.Background(), job.PRKey)
	s.mu.Unlock()
	s.Wake()
}
