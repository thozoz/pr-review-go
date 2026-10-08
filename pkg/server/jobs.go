package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/retry"
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

// deferCap resolves the maximum park duration for a deferred attempt: the
// configured DeferMaxWait when available, otherwise the package default.
func (e *ServerJobExecutor) deferCap() time.Duration {
	if e.server != nil && e.server.cfg != nil && e.server.cfg.DeferMaxWait > 0 {
		return e.server.cfg.DeferMaxWait
	}
	return config.DefaultDeferMaxWait
}

// budgetWait extracts the required retry wait when err signals that the
// worker-useful retry budget is exhausted. ok is false for any other error.
func budgetWait(err error) (wait time.Duration, ok bool) {
	var budgetErr *retry.ExceedsWorkerBudgetError
	if errors.As(err, &budgetErr) {
		return budgetErr.RequiredWait, true
	}
	if errors.Is(err, retry.ErrJobBudgetExhausted) {
		return 0, true
	}
	return 0, false
}

// deferForExhaustedBudget parks the job as a durable deferred attempt due at
// now + min(requiredWait, cap) and frees the worker: the job returns to
// queued (ClaimNextJob skips it until due) and the caller must return nil so
// the scheduler releases the PR slot. A visible waiting notice reuses the
// same owned status comment; its failure is cosmetic and never fails the
// deferral itself. On claim the executor's normal pre-generation head checks
// retarget moved heads, so deferred attempts always resume with latest-head
// semantics and pre-publication head/generation gates still apply.
func (e *ServerJobExecutor) deferForExhaustedBudget(ctx context.Context, job *Job, requiredWait time.Duration, reason string) error {
	cap := e.deferCap()
	wait := requiredWait
	if wait <= 0 || wait > cap {
		wait = cap
	}
	due := time.Now().UTC().Add(wait)
	deferred, err := e.store.DeferJob(ctx, job.ID, due, reason)
	if err != nil {
		return fmt.Errorf("persist deferred attempt: %w", err)
	}
	*job = *deferred
	log.Printf("[jobs] Deferred %s until %s (%s); worker released", job.ID, due.Format(time.RFC3339), reason)
	if e.server != nil && e.server.gh != nil {
		pub := NewPublication(e.store, e.server.gh)
		if _, serr := pub.PublishStatus(ctx, job, fmt.Sprintf("⏳ Rate limited; retry deferred until %s. Worker released; review resumes automatically.", due.Format(time.RFC3339))); serr != nil {
			log.Printf("[jobs] Deferred-status notice for %s failed (continuing): %v", job.ID, serr)
		}
	}
	return nil
}

func (e *ServerJobExecutor) executeReviewJob(ctx context.Context, job *Job) error {
	gh := e.server.gh
	engine := e.server.engine
	pub := NewPublication(e.store, gh)

	// Check if this job already has a saved review output intent.
	// If output was already generated before a crash, reconcile durable saved output
	// before scheduling regeneration — known stored output never reruns generation.
	reviewMarker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
	existingIntent, intentErr := e.store.GetOutputIntent(ctx, reviewMarker)
	if intentErr != nil && !errors.Is(intentErr, ErrIntentNotFound) {
		// An unreadable ledger must never start paid generation.
		log.Printf("[jobs] Failed reading saved review intent for %s: %v", job.ID, intentErr)
		return fmt.Errorf("read saved review intent: %w", intentErr)
	}
	if intentErr == nil && existingIntent != nil && existingIntent.Body != "" {
		log.Printf("[jobs] Reconciling existing stored review output for %s without regeneration", job.ID)
		return e.reconcileAndFinish(ctx, pub, job, existingIntent, false)
	}

	// 1. Initial head check before expensive generation
	livePR, prErr := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr != nil {
		log.Printf("[jobs] Failed fetching live PR for %s: %v", job.ID, prErr)
		if wait, ok := budgetWait(prErr); ok {
			return e.deferForExhaustedBudget(ctx, job, wait, fmt.Sprintf("retry wait exceeds worker budget: %v", prErr))
		}
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

	if job.StatusCommentID == 0 {
		if fresh, err := e.store.GetJob(ctx, job.ID); err == nil && fresh != nil && fresh.StatusCommentID > 0 {
			job.StatusCommentID = fresh.StatusCommentID
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

	if job.StatusCommentID == 0 || isRerun {
		if err := e.publishStatus(ctx, pub, job, initialStatusBody); err != nil {
			return err
		}
	}
	if !isRerun {
		// Transition status comment to running
		if err := e.publishStatus(ctx, pub, job, "🔄 Review running..."); err != nil {
			return err
		}
	}

	// 3. Head-bound review execution
	report, err := engine.ReviewPRAtHead(ctx, job.Owner, job.Repo, job.PRNumber, job.HeadSHA)
	if err != nil {
		log.Printf("[jobs] Review failed for %s: %v", job.ID, err)
		if wait, ok := budgetWait(err); ok {
			return e.deferForExhaustedBudget(ctx, job, wait, fmt.Sprintf("retry wait exceeds worker budget: %v", err))
		}
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
		// No remote review write without a committed recoverable intent. The job
		// stays nonterminal (never completed) so restart recovery owns it.
		log.Printf("[jobs] Failed saving review output intent for %s: %v", job.ID, err)
		return fmt.Errorf("save review output intent: %w", err)
	}

	// 6. Reconcile review output publication across crash windows & in-flight head movement
	return e.reconcileAndFinish(ctx, pub, job, reviewIntent, isRerun)
}

// publishStatus publishes a status transition. Durable-ledger failures abort the
// job (nonterminal, preserved for recovery); remote-only status failures are
// logged since the comment is cosmetic and the saved intent keeps it recoverable.
func (e *ServerJobExecutor) publishStatus(ctx context.Context, pub *Publication, job *Job, text string) error {
	id, err := pub.PublishStatus(ctx, job, text)
	if id > 0 {
		job.StatusCommentID = id
	}
	if err != nil {
		if errors.Is(err, ErrPublicationPersistence) {
			log.Printf("[jobs] Status publication ledger failure for %s: %v", job.ID, err)
			return err
		}
		log.Printf("[jobs] Status publication for %s failed (continuing): %v", job.ID, err)
	}
	return nil
}

// reconcileAndFinish reconciles a saved review output and propagates every
// persistence/uncertainty error instead of treating it as success.
func (e *ServerJobExecutor) reconcileAndFinish(ctx context.Context, pub *Publication, job *Job, intent *OutputIntent, isRerun bool) error {
	res, recErr := pub.ReconcileOutput(ctx, intent)
	if recErr != nil {
		return recErr
	}
	if res == nil {
		return errors.New("publication reconcile returned no result")
	}
	// Refresh terminal fields so later status writes cannot clobber the committed job.
	if fresh, err := e.store.GetJob(ctx, job.ID); err == nil && fresh != nil {
		job.Status = fresh.Status
		job.FinishedAt = fresh.FinishedAt
		job.Error = fresh.Error
		if fresh.StatusCommentID > 0 {
			job.StatusCommentID = fresh.StatusCommentID
		}
	}
	switch res.Status {
	case "uncertain":
		if res.Error != nil {
			return fmt.Errorf("review publication uncertain: %w", res.Error)
		}
		return errors.New("review publication uncertain")
	case "superseded":
		if job.StatusCommentID > 0 {
			return e.publishStatus(ctx, pub, job, "⏭️ Review superseded by newer commit.")
		}
		return nil
	}
	if job.StatusCommentID > 0 && !isRerun {
		return e.publishStatus(ctx, pub, job, "✅ Review completed.")
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

// markNeedsAttention records an unprovable write outcome as needs_attention
// via the existing operator resolve path: the job stays nonterminal with no
// FinishedAt, and only its own PR is blocked per HasBlockedAction semantics.
func (e *ServerJobExecutor) markNeedsAttention(ctx context.Context, job *Job, reason string) {
	job.Status = "needs_attention"
	job.Error = reason
	job.FinishedAt = nil
	if err := e.store.UpdateJob(ctx, job); err != nil {
		log.Printf("[jobs] Failed persisting needs_attention for %s: %v", job.ID, err)
		return
	}
	st, err := e.store.GetPRState(ctx, job.PRKey)
	if err != nil {
		if !errors.Is(err, ErrPRNotFound) {
			log.Printf("[jobs] Failed reading PR state for %s: %v", job.ID, err)
			return
		}
		st = &PRState{PRKey: job.PRKey, Owner: job.Owner, Repo: job.Repo, Number: job.PRNumber}
	}
	st.HasBlockedAction = true
	st.BlockedReason = fmt.Sprintf("job %s is needs_attention", job.ID)
	if err := e.store.UpdatePRState(ctx, st); err != nil {
		log.Printf("[jobs] Failed marking PR %s blocked for %s: %v", job.PRKey, job.ID, err)
	}
}

func (e *ServerJobExecutor) executeImproveJob(ctx context.Context, job *Job) error {
	const message = "## PR Improvement Suggestions\n\nOne-click suggestions are unavailable until isolated project verification is configured. No build or tests were run."
	err := e.server.gh.PostComment(ctx, job.Owner, job.Repo, job.PRNumber, message)
	if err != nil {
		// An ambiguous write must never be retried blindly: without owned
		// proof the outcome is unprovable, so the job becomes
		// needs_attention for operator resolution via the existing
		// --queue-resolve path. Exactly one remote create was attempted.
		if ghclient.IsUncertainWriteError(err) {
			e.markNeedsAttention(ctx, job, fmt.Sprintf("uncertain comment write; requires operator resolution: %v", err))
			return err
		}
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
	// deferPollInterval bounds the idle wake when deferred attempts are
	// parked: workers sleep at most this long before re-checking due jobs.
	deferPollInterval time.Duration
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
		store:             store,
		executor:          executor,
		workers:           workers,
		activePRs:         make(map[string]bool),
		deferPollInterval: config.DefaultDeferPollInterval,
		wakeCh:            make(chan struct{}, 1),
		stopCh:            make(chan struct{}),
	}
}

// SetDeferPollInterval overrides the bounded idle poll for due deferred
// attempts. Non-positive values keep the current setting.
func (s *Scheduler) SetDeferPollInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferPollInterval = d
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

	// Initial store maintenance pass
	if _, err := s.store.MaintainTerminalRecords(ctx, time.Now().UTC(), 128); err != nil {
		log.Printf("[scheduler] Initial store maintenance warning: %v", err)
	}

	s.wg.Add(1)
	go s.maintenanceLoop(ctx)

	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.workerLoop(ctx, i)
	}

	// Trigger initial work check
	s.Wake()
	return nil
}

func (s *Scheduler) maintenanceLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.store.MaintainTerminalRecords(ctx, time.Now().UTC(), 128)
		}
	}
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
			case <-time.After(s.idleWait(ctx)):
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

// idleWait bounds how long a worker sleeps when no job is claimable. With no
// parked deferral it stays at the fast 100ms poll; when deferred attempts are
// waiting it sleeps until the earliest due time, capped by the configured
// poll interval, so due jobs resume promptly without hot-spinning.
func (s *Scheduler) idleWait(ctx context.Context) time.Duration {
	const fastPoll = 100 * time.Millisecond
	s.mu.Lock()
	poll := s.deferPollInterval
	s.mu.Unlock()
	if poll <= 0 {
		poll = config.DefaultDeferPollInterval
	}
	due, ok, err := s.store.EarliestDeferredDue(ctx)
	if err != nil || !ok {
		return fastPoll
	}
	until := time.Until(due)
	if until <= 0 {
		// Due but unclaimable (blocked PR or active worker): back off to the
		// bounded poll instead of spinning.
		return poll
	}
	if until > poll {
		return poll
	}
	return until
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
