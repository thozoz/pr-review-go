package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
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
	var timeout time.Duration
	switch job.Kind {
	case "review":
		timeout = 10 * time.Minute
	case "labels", "summary", "improve":
		timeout = 2 * time.Minute
	case "describe", "changelog", "docs":
		timeout = 3 * time.Minute
	case "assistant":
		timeout = 5 * time.Minute
	default:
		timeout = 5 * time.Minute
	}

	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if job.Trigger == "explicit" && job.Author != "" && e.server != nil && e.server.gh != nil {
		authCtx, authCancel := context.WithTimeout(jobCtx, 5*time.Second)
		allowed, err := e.server.gh.CanWriteRepository(authCtx, job.Owner, job.Repo, job.Author)
		authCancel()
		if err != nil || !allowed {
			log.Printf("[jobs] Reauthorization failed for explicit command %s by %q on %s/%s: allowed=%v err=%v",
				job.ID, job.Author, job.Owner, job.Repo, allowed, err)
			job.Status = "failed"
			job.Error = "actor permission revoked or unavailable"
			now := time.Now().UTC()
			job.FinishedAt = &now
			_ = e.store.UpdateJob(jobCtx, job)
			return fmt.Errorf("reauthorization denied: actor %s", job.Author)
		}
	}

	if e.server != nil && e.server.dispatchHook != nil {
		e.server.dispatchHook(job.Kind, job.Owner, job.Repo, job.PRNumber)
	}

	switch job.Kind {
	case "review":
		return e.executeReviewJob(jobCtx, job)
	case "labels":
		return e.executeLabelsJob(jobCtx, job)
	case "describe":
		return e.executeDescribeJob(jobCtx, job)
	case "improve":
		return e.executeImproveJob(jobCtx, job)
	case "summary":
		return e.executeSummaryJob(jobCtx, job)
	case "changelog":
		return e.executeChangelogJob(jobCtx, job)
	case "docs":
		return e.executeDocsJob(jobCtx, job)
	case "assistant":
		return e.executeAssistantJob(jobCtx, job)
	default:
		job.Status = "failed"
		job.Error = fmt.Sprintf("unknown job kind: %s", job.Kind)
		return e.store.UpdateJob(jobCtx, job)
	}
}

func (e *ServerJobExecutor) executeReviewJob(ctx context.Context, job *Job) error {
	gh := e.server.gh
	engine := e.server.engine
	pub := NewPublication(e.store, gh)

	// Check if this job already has a saved review output intent.
	// If output was already generated before a crash, reconcile durable saved output
	// before scheduling regeneration — known stored output never reruns generation.
	reviewMarker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
	existingIntent, _ := e.store.GetOutputIntent(ctx, reviewMarker)
	if existingIntent != nil && existingIntent.Body != "" {
		log.Printf("[jobs] Reconciling existing stored review output for %s without regeneration", job.ID)
		res, err := pub.ReconcileOutput(ctx, existingIntent)
		if err != nil {
			return err
		}
		if res != nil && res.Status == "completed" && job.StatusCommentID > 0 {
			_, _ = pub.PublishStatus(ctx, job, "✅ Review completed.")
		}
		return nil
	}

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

	_, _ = pub.PublishStatus(ctx, job, initialStatusBody)
	if !isRerun {
		// Transition status comment to running
		_, _ = pub.PublishStatus(ctx, job, "🔄 Review running...")
	}

	// 3. Head-bound review execution
	report, err := engine.ReviewPRAtHead(ctx, job.Owner, job.Repo, job.PRNumber, job.HeadSHA)
	if err != nil {
		log.Printf("[jobs] Review failed for %s: %v", job.ID, err)
		if job.StatusCommentID > 0 {
			_, _ = pub.PublishStatus(ctx, job, fmt.Sprintf("❌ Review failed: %v", err))
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
	reviewMarker = fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
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

	// 6. Reconcile review output publication across crash windows & in-flight head movement
	res, recErr := pub.ReconcileOutput(ctx, reviewIntent)
	if recErr != nil || (res != nil && res.Status == "uncertain") {
		return recErr
	}

	if res != nil && res.Status == "superseded" {
		if job.StatusCommentID > 0 {
			_, _ = pub.PublishStatus(ctx, job, "⏭️ Review superseded by newer commit.")
		}
		return nil
	}

	// 7. Update status comment to completed
	if job.StatusCommentID > 0 && !isRerun {
		_, _ = pub.PublishStatus(ctx, job, "✅ Review completed.")
	}

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
	if e.server != nil {
		e.server.dispatchAddDocs(ctx, job.Owner, job.Repo, job.PRNumber)
	}
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
	store       JobStore
	executor    JobExecutor
	workers     int
	activePRs   map[string]bool
	llmGate     llm.RequestGate
	sandboxGate sandbox.AdmissionGate
	mu          sync.Mutex
	wakeCh      chan struct{}
	stopCh      chan struct{}
	stopped     bool
	wg          sync.WaitGroup
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

// SetLLMGate assigns a shared LLM RequestGate to the scheduler.
func (s *Scheduler) SetLLMGate(gate llm.RequestGate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.llmGate = gate
}

// LLMGate returns the scheduler's shared LLM RequestGate.
func (s *Scheduler) LLMGate() llm.RequestGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.llmGate
}

// SetSandboxGate assigns a shared sandbox AdmissionGate to the scheduler.
func (s *Scheduler) SetSandboxGate(gate sandbox.AdmissionGate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxGate = gate
}

// SandboxGate returns the scheduler's shared sandbox AdmissionGate.
func (s *Scheduler) SandboxGate() sandbox.AdmissionGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxGate
}

func (s *Scheduler) Start(ctx context.Context) error {
	// Recover interrupted jobs on startup
	if _, err := s.store.RecoverJobs(ctx); err != nil {
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

func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	close(s.stopCh)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scheduler shutdown timed out: %w", ctx.Err())
	}
}

func (s *Scheduler) Stop() {
	_ = s.Shutdown(context.Background())
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

		execCtx := ctx
		s.mu.Lock()
		gate := s.llmGate
		sGate := s.sandboxGate
		s.mu.Unlock()
		if gate != nil {
			execCtx = llm.WithAdmission(execCtx, gate)
		}
		if sGate != nil {
			execCtx = sandbox.WithAdmission(execCtx, sGate)
		}

		_ = s.executor.ExecuteJob(execCtx, job)
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
