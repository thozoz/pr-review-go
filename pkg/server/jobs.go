package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thozoz/pr-review-go/pkg/assistant"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
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
	ledger *DecisionLedger

	// Edit-pipeline seams. Nil means production behavior; tests inject fakes.
	// They live on the executor (not Server) so parallel executors stay isolated.
	editGH       editGitHub
	editLLM      LLMCaller
	editClassify func(ctx context.Context, llm LLMCaller, body string) (IntentVerdict, error)
	editPrepare  func(ctx context.Context, cloneURL, headRef, headSHA string) (*sandbox.Snapshot, func(), error)
	editLoop     func(ctx context.Context, workDir, instruction, stickyContext string) (assistant.EditResult, error)
	editVerify   func(ctx context.Context, snap *sandbox.Snapshot) (*sandbox.VerificationReport, error)
	editPublish  func(ctx context.Context, job *Job, text string) error
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
	case "edit":
		// Edit jobs hold the per-PR slot for the whole gated pipeline
		// (snapshot, LLM edit loop, verify, push); 10 minutes matches review.
		timeout = 10 * time.Minute
	case "labels", "summary", "improve":
		timeout = 2 * time.Minute
	case "describe", "changelog", "docs", "approve", "request_changes":
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
	case "approve":
		return e.executeApproveJob(jobCtx, job)
	case "request_changes":
		return e.executeRequestChangesJob(jobCtx, job)
	case "assistant":
		return e.executeAssistantJob(jobCtx, job)
	case "edit":
		// Edit jobs hold the per-PR slot for the whole gated pipeline while
		// other PRs proceed; concurrent edits on one PR serialize through
		// ClaimNextJob. Slot-hold lifecycle is documented in plan 05-05 docs.
		return e.executeEditJob(jobCtx, job)
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

func (e *ServerJobExecutor) SetDecisionLedger(l *DecisionLedger) {
	e.ledger = l
}

func (e *ServerJobExecutor) getDecisionLedger() *DecisionLedger {
	if e.ledger != nil {
		return e.ledger
	}
	if e.server != nil {
		if l := e.server.getDecisionLedger(); l != nil {
			return l
		}
	}
	if bStore, ok := e.store.(*BoltJobStore); ok && bStore != nil {
		return NewDecisionLedger(bStore.DB())
	}
	return nil
}

func (e *ServerJobExecutor) executeApproveJob(ctx context.Context, job *Job) error {
	return e.executeDecisionJob(ctx, job, ghclient.EventApprove)
}

func (e *ServerJobExecutor) executeRequestChangesJob(ctx context.Context, job *Job) error {
	return e.executeDecisionJob(ctx, job, ghclient.EventRequestChanges)
}

func (e *ServerJobExecutor) executeDecisionJob(ctx context.Context, job *Job, event string) error {
	if e.server == nil || e.server.gh == nil {
		job.Status = "failed"
		job.Error = "github client unavailable"
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return errors.New("github client unavailable")
	}

	gh := e.server.gh
	pub := NewPublication(e.store, gh)
	ledger := e.getDecisionLedger()
	if ledger == nil {
		job.Status = "failed"
		job.Error = "decision ledger unavailable"
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return errors.New("decision ledger unavailable")
	}

	// 1. Initial head check before generation
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

	// 2. Void and notify if job head differs from live head (D-04)
	if livePR.HeadSHA != job.HeadSHA {
		log.Printf("[jobs] Decision head mismatch for %s: job was for %s, live is %s; voiding decision", job.ID, job.HeadSHA, livePR.HeadSHA)
		notice := fmt.Sprintf("⏭️ Decision (%s) for commit %s voided by newer commit %s. A fresh decision command is required.", event, job.HeadSHA, livePR.HeadSHA)
		if err := e.publishStatus(ctx, pub, job, notice); err != nil {
			log.Printf("[jobs] Failed publishing void status for %s: %v", job.ID, err)
		}
		job.HeadSHA = livePR.HeadSHA
		job.BaseSHA = livePR.BaseSHA
		job.Status = "superseded"
		job.Error = fmt.Sprintf("voided by newer commit %s", livePR.HeadSHA)
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return nil
	}

	// 3. Pre-generation duplicate check
	if existing, _ := ledger.GetDecision(ctx, job.PRKey, job.HeadSHA); existing != nil {
		log.Printf("[jobs] Duplicate decision command for %s (head %s already recorded by job %s); rejecting with warning", job.ID, job.HeadSHA, existing.JobID)
		warningMsg := fmt.Sprintf("⚠️ A decision review has already been submitted for commit %s. Duplicate %s command skipped.", job.HeadSHA, event)
		_ = gh.PostComment(ctx, job.Owner, job.Repo, job.PRNumber, warningMsg)
		if job.StatusCommentID > 0 {
			_ = e.publishStatus(ctx, pub, job, fmt.Sprintf("⚠️ Duplicate %s command for commit %s; skipped.", event, job.HeadSHA[:8]))
		}
		job.Status = "failed"
		job.Error = fmt.Sprintf("decision already recorded for head %s (job %s)", job.HeadSHA, existing.JobID)
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return nil
	}

	// 4. Update status comment to running
	if err := e.publishStatus(ctx, pub, job, fmt.Sprintf("🔄 Decision (%s) running...", event)); err != nil {
		return err
	}

	// 5. Generate review report
	var report *reviewer.ReviewReport
	if e.server != nil && e.server.engine != nil {
		var rerr error
		report, rerr = e.server.engine.ReviewPRAtHead(ctx, job.Owner, job.Repo, job.PRNumber, job.HeadSHA)
		if rerr != nil {
			log.Printf("[jobs] Decision review generation failed for %s: %v", job.ID, rerr)
			if wait, ok := budgetWait(rerr); ok {
				return e.deferForExhaustedBudget(ctx, job, wait, fmt.Sprintf("retry wait exceeds worker budget: %v", rerr))
			}
			if job.StatusCommentID > 0 {
				_, _ = pub.PublishStatus(ctx, job, fmt.Sprintf("❌ Decision failed: %v", rerr))
			}
			job.Status = "failed"
			job.Error = rerr.Error()
			now := time.Now().UTC()
			job.FinishedAt = &now
			_ = e.store.UpdateJob(ctx, job)
			return rerr
		}
	}

	// 6. Pre-publication live head check (D-04, D-16)
	livePR2, prErr2 := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr2 == nil && livePR2 != nil && livePR2.HeadSHA != job.HeadSHA {
		log.Printf("[jobs] Head changed during decision generation for %s (job=%s, live=%s); voiding decision", job.ID, job.HeadSHA, livePR2.HeadSHA)
		notice := fmt.Sprintf("⏭️ Decision (%s) for commit %s voided by newer commit %s. A fresh decision command is required.", event, job.HeadSHA, livePR2.HeadSHA)
		_ = e.publishStatus(ctx, pub, job, notice)
		job.Status = "superseded"
		job.HeadSHA = livePR2.HeadSHA
		job.BaseSHA = livePR2.BaseSHA
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return nil
	}

	// 7. Format body and suggestions
	body := fmt.Sprintf("## PR Decision: %s\n\nReviewed at commit %s.", event, job.HeadSHA)
	var suggestions []ghclient.InlineSuggestion
	if report != nil {
		prState, _ := e.store.GetPRState(ctx, job.PRKey)
		var priorFindings []reviewer.PriorFinding
		if prState != nil && prState.LastReviewedHead != "" && prState.LastReviewedHead != job.HeadSHA {
			priorFindings = reviewer.GetFindings(job.PRKey.String(), prState.LastReviewedHead)
		}
		if len(priorFindings) > 0 {
			cls := reviewer.ClassifyAgainstPrior(report.Findings, priorFindings[0].HeadSHA, priorFindings)
			report.Classification = &cls
			report.PersistingFindings = cls.Persisting
			report.FixedFindings = cls.Fixed
			report.NewFindings = cls.New
			report.RawMarkdown = reviewer.FormatReportMarkdown(report)
		}

		if report.RawMarkdown != "" {
			body = report.RawMarkdown
		} else {
			body = reviewer.FormatReportMarkdown(report)
		}
		suggestions = report.Suggestions
	}

	// 8. Publish decision (atomic ClaimDecision under per-key mutex, single remote CreateReview, status-intent escalation)
	res, pubErr := PublishDecision(ctx, ledger, gh, job, event, body, suggestions, e.markNeedsAttention)
	if pubErr != nil {
		if res != nil && res.Duplicate {
			warningMsg := fmt.Sprintf("⚠️ A decision review has already been submitted for commit %s. Duplicate %s command skipped.", job.HeadSHA, event)
			_ = gh.PostComment(ctx, job.Owner, job.Repo, job.PRNumber, warningMsg)
			if job.StatusCommentID > 0 {
				_ = e.publishStatus(ctx, pub, job, fmt.Sprintf("⚠️ Duplicate %s command for commit %s; skipped.", event, job.HeadSHA[:8]))
			}
			job.Status = "failed"
			job.Error = fmt.Sprintf("decision already recorded for head %s", job.HeadSHA)
			now := time.Now().UTC()
			job.FinishedAt = &now
			_ = e.store.UpdateJob(ctx, job)
			return nil
		}
		if ghclient.IsUncertainWriteError(pubErr) {
			return pubErr
		}
		job.Status = "failed"
		job.Error = pubErr.Error()
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = e.store.UpdateJob(ctx, job)
		return pubErr
	}
	_ = res

	// Record findings for future re-review classification
	if report != nil && len(report.Findings) > 0 {
		reviewer.RecordFindings(job.PRKey.String(), job.HeadSHA, report.Findings)
	}

	// 9. Update recycled status comment to completed (D-16)
	shaShort := job.HeadSHA
	if len(shaShort) > 8 {
		shaShort = shaShort[:8]
	}
	successText := fmt.Sprintf("✅ Decision %s published for commit %s", event, shaShort)
	_ = e.publishStatus(ctx, pub, job, successText)

	job.Status = "completed"
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = e.store.UpdateJob(ctx, job)
	return nil
}

// ---------------------------------------------------------------------------
// Edit executor (plan 05-04): gated same-PR edit pipeline.
//
// The nine steps inside executeEditJob run in locked order with no step
// reordered:
//  1. ClassifyEditIntent first (englishSummary captured from the verdict
//     Summary field); question/unclear/low-confidence resolves to a
//     multilingual confirm comment with zero snapshot work.
//  2. Live-head GetPR with the shared budgetWait/defer path; a malformed
//     live head fails closed.
//  3. Fork refusal (IsFork, fail-closed on missing metadata) with zero work.
//  4. Status transition to editing via the recycled owned comment.
//  5. Sealed PrepareSnapshot at exactly job.HeadSHA (honest-degradation
//     unavailable notice on ErrSourceProviderUnavailable), sticky context
//     (EditHistory plus prior bot diff capped at 60k) injected into
//     RunGatedEditLoop under DefaultEditCaps.
//  6. Status verifying; Runner.RunSnapshot on the edited tree; only
//     StatusPassed proceeds. Green-time bytes are snapshotted into an
//     in-memory map; the work copy is never re-walked afterward.
//  7. Push-time CanWriteRepository re-verification (fresh 5s timeout,
//     fail-closed) immediately before minting the token.
//  8. Second live-head GetPR (stale aborts superseded with zero push),
//     single CreatePushCredential plus exactly one CommitFilesAtExpectedHead
//     CAS call (revoked in defer); HeadMovedError, 403/422 refusals, and
//     uncertain outcomes map per D-31, D-25, D-34 with zero retries.
//  9. Sticky persistence (LastBotCommitSHA/BaseSHA plus capped history) and
//     the final summary comment (files, verification result, commit SHA).
//
// Token containment (T-05-02): the push credential token is passed only to
// NewScopedClient inside liveEditGitHub.CommitPush and never reaches the edit
// loop or prompts. Transport (T-05-04): only CAS GraphQL, no force parameter.
// ---------------------------------------------------------------------------

const (
	// editPriorDiffMaxBytes caps sticky prior-diff injection into the edit loop.
	editPriorDiffMaxBytes = 60000
	// editPriorDiffTruncatedMarker marks a capped prior diff.
	editPriorDiffTruncatedMarker = "\n...[truncated: prior diff exceeds 60000 bytes]..."
	// editVerifyMaxAttempts bounds the ReAct verify-failure retry loop
	// (initial attempt plus self-corrections).
	editVerifyMaxAttempts = 3
	// editVerifyFeedbackMaxBytes caps failure output fed back to the model.
	editVerifyFeedbackMaxBytes = 6000
)

// isRetryableVerifyStatus reports whether a failed verification is worth a
// model self-correction attempt: actionable build/test/timeout output.
// Unavailable, unsupported, and incomplete outcomes never improve on retry.
func isRetryableVerifyStatus(status sandbox.VerificationStatus) bool {
	switch status {
	case sandbox.StatusBuildFailed, sandbox.StatusTestFailed, sandbox.StatusTimeout:
		return true
	default:
		return false
	}
}

// editVerifyFeedback renders the failing verification as capped model input:
// status, reason, and the stderr of each failed stage.
func editVerifyFeedback(report *sandbox.VerificationReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "verification status: %s\nreason: %s", report.Status, report.Reason)
	for _, res := range report.Results {
		if res.Passed {
			continue
		}
		stderr := res.Stderr
		if len(stderr) > editVerifyFeedbackMaxBytes {
			stderr = stderr[:editVerifyFeedbackMaxBytes] + "\n...[truncated: failure output exceeds 6000 bytes]..."
		}
		fmt.Fprintf(&sb, "\n\nfailed stage %q (exit %d):\n%s", res.Command, res.ExitCode, stderr)
		if sb.Len() > editVerifyFeedbackMaxBytes*2 {
			break
		}
	}
	out := sb.String()
	if len(out) > editVerifyFeedbackMaxBytes*2 {
		out = out[:editVerifyFeedbackMaxBytes*2] + "\n...[truncated]"
	}
	return out
}

// editGitHub is the narrow GitHub surface the edit executor needs. The live
// adapter routes the single push through a single-use scoped client; tests
// inject a counting fake.
type editGitHub interface {
	GetPR(ctx context.Context, owner, repo string, number int) (*ghclient.PRDetails, error)
	CanWriteRepository(ctx context.Context, owner, repo, username string) (bool, error)
	GetDiffAtCommits(ctx context.Context, owner, repo, baseOID, headOID string) (string, error)
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
	CreatePushCredential(ctx context.Context, owner, repo string, repoID int64) (*ghclient.PushCredential, error)
	RevokePushCredential(ctx context.Context, cred *ghclient.PushCredential) error
	CommitPush(ctx context.Context, cred *ghclient.PushCredential, owner, repo, branchName, expectedHeadOID, headline string, files map[string]string, deletions []string) (string, error)
}

// liveEditGitHub adapts *ghclient.Client to editGitHub.
type liveEditGitHub struct {
	c *ghclient.Client
}

func (l *liveEditGitHub) GetPR(ctx context.Context, owner, repo string, number int) (*ghclient.PRDetails, error) {
	return l.c.GetPR(ctx, owner, repo, number)
}

func (l *liveEditGitHub) CanWriteRepository(ctx context.Context, owner, repo, username string) (bool, error) {
	return l.c.CanWriteRepository(ctx, owner, repo, username)
}

func (l *liveEditGitHub) GetDiffAtCommits(ctx context.Context, owner, repo, baseOID, headOID string) (string, error) {
	return l.c.GetDiffAtCommits(ctx, owner, repo, baseOID, headOID)
}

func (l *liveEditGitHub) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	return l.c.PostComment(ctx, owner, repo, number, body)
}

func (l *liveEditGitHub) CreatePushCredential(ctx context.Context, owner, repo string, repoID int64) (*ghclient.PushCredential, error) {
	return l.c.CreatePushCredential(ctx, owner, repo, repoID)
}

func (l *liveEditGitHub) RevokePushCredential(ctx context.Context, cred *ghclient.PushCredential) error {
	return l.c.RevokePushCredential(ctx, cred)
}

// CommitPush performs exactly one CAS commit through a single-use scoped
// client minted from the credential token. The token never reaches the edit
// loop or prompts (T-05-02).
func (l *liveEditGitHub) CommitPush(ctx context.Context, cred *ghclient.PushCredential, owner, repo, branchName, expectedHeadOID, headline string, files map[string]string, deletions []string) (string, error) {
	if cred == nil || cred.Token == "" {
		return "", errors.New("push credential unavailable")
	}
	scoped := l.c.NewScopedClient(cred.Token)
	return scoped.CommitFilesAtExpectedHead(ctx, owner, repo, branchName, expectedHeadOID, headline, files, deletions)
}

// buildEditStickyContext assembles the instruction history plus the prior bot
// diff for the gated edit loop. Empty on a first edit.
func buildEditStickyContext(history []string, priorDiff string) string {
	var sb strings.Builder
	if len(history) > 0 {
		sb.WriteString("Previous edit instructions on this PR (oldest first):\n")
		for i, h := range history {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, h)
		}
		sb.WriteString("\n")
	}
	if priorDiff != "" {
		sb.WriteString("Diff of the previous bot edit (context only; do not revert unless asked):\n")
		sb.WriteString(priorDiff)
		if !strings.HasSuffix(priorDiff, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// shortCommitSHA returns the first 7 hex chars for display; the input is
// already OID-validated at every call site.
func shortCommitSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

// short8 returns the first 8 chars for stale-abort comments (decision-job convention).
func short8(sha string) string {
	if len(sha) >= 8 {
		return sha[:8]
	}
	return sha
}

// isPushRefusedError reports permanent push rejections (auth/permission) that
// must surface as refuse-with-reason comments, never bypasses (D-25).
func isPushRefusedError(err error) bool {
	if err == nil {
		return false
	}
	var hme *ghclient.HeadMovedError
	if errors.As(err, &hme) {
		return false
	}
	if ghclient.IsUncertainWriteError(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "403") || strings.Contains(msg, "422") ||
		strings.Contains(msg, "forbidden") || strings.Contains(msg, "permission")
}

func (e *ServerJobExecutor) failEditJob(ctx context.Context, job *Job, msg string) error {
	job.Status = "failed"
	job.Error = msg
	now := time.Now().UTC()
	job.FinishedAt = &now
	if e.store == nil {
		return errors.New(msg)
	}
	if uerr := e.store.UpdateJob(ctx, job); uerr != nil {
		log.Printf("[jobs] Failed persisting failed edit job %s: %v", job.ID, uerr)
	}
	if msg == "" {
		return errors.New("edit job failed")
	}
	return errors.New(msg)
}

func (e *ServerJobExecutor) completeEditJob(ctx context.Context, job *Job) error {
	job.Status = "completed"
	job.Error = ""
	now := time.Now().UTC()
	job.FinishedAt = &now
	if e.store == nil {
		return errors.New("job store unavailable")
	}
	if uerr := e.store.UpdateJob(ctx, job); uerr != nil {
		log.Printf("[jobs] Failed persisting completed edit job %s: %v", job.ID, uerr)
		return uerr
	}
	return nil
}

func (e *ServerJobExecutor) supersedeEditJob(ctx context.Context, job *Job, reason string) error {
	job.Status = "superseded"
	job.Error = reason
	now := time.Now().UTC()
	job.FinishedAt = &now
	if e.store == nil {
		return errors.New("job store unavailable")
	}
	if uerr := e.store.UpdateJob(ctx, job); uerr != nil {
		log.Printf("[jobs] Failed persisting superseded edit job %s: %v", job.ID, uerr)
		return uerr
	}
	return nil
}

func (e *ServerJobExecutor) executeEditJob(ctx context.Context, job *Job) error {
	if e.server == nil {
		return e.failEditJob(ctx, job, "server unavailable")
	}
	if e.store == nil {
		return errors.New("job store unavailable")
	}

	// Resolve the GitHub surface: injected fake in tests, live adapter in prod.
	gh := e.editGH
	var live *ghclient.Client
	if gh == nil {
		live = e.server.gh
		if live == nil {
			return e.failEditJob(ctx, job, "github client unavailable")
		}
		gh = &liveEditGitHub{c: live}
	}

	llmClient := e.editLLM
	if llmClient == nil && e.server.cfg != nil {
		llmClient = llm.NewClient(e.server.cfg.LLMBaseURL, e.server.cfg.LLMAPIKey, e.server.cfg.LLMModel)
	}
	classify := e.editClassify
	if classify == nil {
		classify = ClassifyEditIntent
	}
	publish := e.editPublish
	if publish == nil {
		if live != nil {
			pub := NewPublication(e.store, live)
			publish = func(pctx context.Context, pjob *Job, text string) error {
				return e.publishStatus(pctx, pub, pjob, text)
			}
		} else {
			// Fake GitHub without an injected publisher: direct comments.
			publish = func(pctx context.Context, pjob *Job, text string) error {
				return gh.PostComment(pctx, pjob.Owner, pjob.Repo, pjob.PRNumber, text)
			}
		}
	}
	// say posts user-facing text best-effort: the durable terminal state is
	// authoritative, so a cosmetic comment failure never flips the outcome.
	say := func(text string) {
		if err := publish(ctx, job, text); err != nil {
			log.Printf("[jobs] Edit comment for %s failed (continuing): %v", job.ID, err)
		}
	}

	prepare := e.editPrepare
	verify := e.editVerify
	if prepare == nil || verify == nil {
		editBackend, editSlots := sandbox.PlatformComponents(e.server.cfg)
		platformRunner := sandbox.NewPlatformRunner(e.server.cfg, live, editBackend, editSlots)
		if prepare == nil {
			prepare = platformRunner.PrepareSnapshot
		}
		if verify == nil {
			verify = platformRunner.RunSnapshot
		}
	}
	loop := e.editLoop
	if loop == nil {
		loop = func(lctx context.Context, workDir, instruction, sticky string) (assistant.EditResult, error) {
			return assistant.RunGatedEditLoop(lctx, llmClient, workDir, instruction, sticky, assistant.DefaultEditCaps())
		}
	}

	// 1) Intent first: the instruction is data, never policy (T-05-01, D-21).
	// The inherited executor reauth at job start already covers admission.
	verdict, cerr := classify(ctx, llmClient, job.Payload)
	englishSummary := ""
	proceed := false
	if cerr == nil {
		englishSummary = strings.TrimSpace(verdict.Summary)
		proceed = verdict.Intent == "commit" && verdict.Confidence >= IntentCommitThreshold
	} else {
		log.Printf("[jobs] Edit intent classification failed for %s: %v", job.ID, cerr)
	}
	if !proceed {
		say("🤔 Bu isteği net anlayamadım — hangi dosyada ne değişmesini istediğinizi daha açık yazar mısınız?\n\nI couldn't confidently determine the requested change — please rephrase with the file and the desired change, then re-request with `/improve --commit` or `@pr-review`.")
		return e.completeEditJob(ctx, job)
	}

	// 2) Live head before any expensive work.
	livePR, prErr := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr != nil {
		log.Printf("[jobs] Edit live-PR fetch failed for %s: %v", job.ID, prErr)
		if wait, ok := budgetWait(prErr); ok {
			return e.deferForExhaustedBudget(ctx, job, wait, fmt.Sprintf("retry wait exceeds worker budget: %v", prErr))
		}
		return e.failEditJob(ctx, job, prErr.Error())
	}
	if err := ghclient.ValidateCommitOID(livePR.HeadSHA); err != nil {
		return e.failEditJob(ctx, job, fmt.Sprintf("invalid live head SHA %s: %v", livePR.HeadSHA, err))
	}

	// 3) Fork refusal before any snapshot work (D-24; nil metadata fails
	// closed because IsFork returns true when head repo metadata is missing).
	if livePR.IsFork() {
		say("🍴 Fork PR'lara otomatik push yapılmaz — değişikliği aynı depodaki bir dala uygulayın.\n\nAutomated pushes to fork PRs are refused; no changes were made. Please apply the change on a branch in this repository.")
		return e.completeEditJob(ctx, job)
	}

	// 4) Status transition to editing via the recycled owned comment.
	say("✏️ Düzenleme çalışıyor... / Editing...")

	if err := ghclient.ValidateCommitOID(job.HeadSHA); err != nil {
		return e.failEditJob(ctx, job, fmt.Sprintf("invalid job head SHA: %v", err))
	}

	// 5) Sealed snapshot at exactly the admitted head; host fallback is
	// structurally absent (D-27).
	snap, cleanup, serr := prepare(ctx, livePR.CloneURL, livePR.HeadRef, job.HeadSHA)
	if serr != nil {
		log.Printf("[jobs] Edit snapshot failed for %s: %v", job.ID, serr)
		if wait, ok := budgetWait(serr); ok {
			return e.deferForExhaustedBudget(ctx, job, wait, fmt.Sprintf("retry wait exceeds worker budget: %v", serr))
		}
		if errors.Is(serr, sandbox.ErrSourceProviderUnavailable) {
			say("🚧 İzole çalışma ortamı şu anda kullanılamıyor — hiçbir değişiklik yapılmadı, daha sonra tekrar deneyin.\n\nIsolated execution is currently unavailable; no changes were made. Please retry later.")
			return e.completeEditJob(ctx, job)
		}
		say(fmt.Sprintf("🚧 Snapshot hazırlanamadı — hiçbir değişiklik yapılmadı: %v\n\nCould not prepare the isolated snapshot; no changes were made: %v", serr, serr))
		return e.failEditJob(ctx, job, serr.Error())
	}
	defer cleanup()

	// Sticky context: instruction history plus the prior bot diff (D-38).
	var history []string
	priorBase := job.BaseSHA
	var priorHead string
	if st, gerr := e.store.GetPRState(ctx, job.PRKey); gerr == nil && st != nil {
		history = append([]string(nil), st.EditHistory...)
		if st.LastBotBaseSHA != "" {
			priorBase = st.LastBotBaseSHA
		}
		priorHead = st.LastBotCommitSHA
	}
	var priorDiff string
	if priorHead != "" && priorHead != priorBase &&
		ghclient.ValidateCommitOID(priorBase) == nil && ghclient.ValidateCommitOID(priorHead) == nil {
		if d, derr := gh.GetDiffAtCommits(ctx, job.Owner, job.Repo, priorBase, priorHead); derr != nil {
			log.Printf("[jobs] Edit prior-diff fetch for %s failed (continuing without): %v", job.ID, derr)
		} else if d != "" {
			priorDiff = d
			if len(priorDiff) > editPriorDiffMaxBytes {
				priorDiff = priorDiff[:editPriorDiffMaxBytes] + editPriorDiffTruncatedMarker
			}
		}
	}
	stickyContext := buildEditStickyContext(history, priorDiff)

	// 5-6) ReAct loop: edit, verify, and on actionable verification failure
	// feed the capped failure output back into the next edit attempt.
	// Bounded to editVerifyMaxAttempts total attempts; only build, test, and
	// timeout failures are retried. The same gate holds: nothing is pushed
	// unless a verification reports StatusPassed.
	var editResult assistant.EditResult
	var report *sandbox.VerificationReport
	attemptFeedback := ""
	attempts := 0
	for {
		attempts++
		sticky := stickyContext
		if attemptFeedback != "" {
			sticky += "\n\nPrevious verification FAILED — fix these errors and nothing else:\n" + attemptFeedback
		}
		var lerr error
		editResult, lerr = loop(ctx, snap.SourceDir, job.Payload, sticky)
		if lerr != nil {
			log.Printf("[jobs] Edit loop failed for %s: %v", job.ID, lerr)
			say(fmt.Sprintf("❌ Düzenleme uygulanamadı — push yapılmadı.\n\nThe isolated edit could not be applied; nothing was pushed: %v", lerr))
			return e.failEditJob(ctx, job, lerr.Error())
		}

		say("🔄 Doğrulama çalışıyor... / Verifying...")
		var verr error
		report, verr = verify(ctx, snap)
		if verr != nil {
			log.Printf("[jobs] Edit verification error for %s: %v", job.ID, verr)
			say(fmt.Sprintf("❌ Doğrulama çalıştırılamadı — push yapılmadı.\n\nVerification could not run; nothing was pushed: %v", verr))
			return e.failEditJob(ctx, job, verr.Error())
		}
		if report == nil {
			return e.failEditJob(ctx, job, "verification returned no report")
		}
		if report.Status == sandbox.StatusUnavailable {
			say("🚧 İzole doğrulama şu anda kullanılamıyor — hiçbir değişiklik push edilmedi, daha sonra tekrar deneyin.\n\nIsolated verification is currently unavailable; nothing was pushed. Please retry later.")
			return e.completeEditJob(ctx, job)
		}
		if report.Status == sandbox.StatusPassed {
			break
		}
		if !isRetryableVerifyStatus(report.Status) || attempts >= editVerifyMaxAttempts {
			say(fmt.Sprintf("❌ Doğrulama geçilemedi (%s) — push yapılmadı.\n\nVerification did not pass (%s); nothing was pushed.", report.Status, report.Status))
			return e.failEditJob(ctx, job, fmt.Sprintf("verification status %s: %s", report.Status, report.Reason))
		}
		attemptFeedback = editVerifyFeedback(report)
		log.Printf("[jobs] Verification failed for %s (%s); retrying edit %d/%d", job.ID, report.Status, attempts+1, editVerifyMaxAttempts)
		say(fmt.Sprintf("🔁 Doğrulama geçilemedi (%s) — hata çıktısı modele geri verildi, düzeltiliyor (deneme %d/%d)...\n\nVerification failed (%s); feeding output back for a fix (attempt %d/%d)...", report.Status, attempts+1, editVerifyMaxAttempts, report.Status, attempts+1, editVerifyMaxAttempts))
	}
	// Green-time byte snapshot: read each changed file exactly once into
	// memory; the work copy is never re-walked after this point (T-05-03).
	files := make(map[string]string, len(editResult.FilesChanged))
	for _, p := range editResult.FilesChanged {
		rel := filepath.Clean(filepath.FromSlash(p))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return e.failEditJob(ctx, job, fmt.Sprintf("refusing to push out-of-jail path %q", p))
		}
		b, rerr := os.ReadFile(filepath.Join(snap.SourceDir, rel))
		if rerr != nil {
			log.Printf("[jobs] Edit green-snapshot read failed for %s path %s: %v", job.ID, p, rerr)
			say("❌ Doğrulanmış dosyalar okunamadı — push yapılmadı.\n\nVerified files could not be read; nothing was pushed.")
			return e.failEditJob(ctx, job, rerr.Error())
		}
		files[p] = string(b)
	}
	if len(files) == 0 {
		say("ℹ️ Düzenleme dosya değişikliği üretmedi — push yapılmadı.\n\nThe edit produced no file changes; nothing was pushed.")
		return e.completeEditJob(ctx, job)
	}

	// 7) Push-time re-verification with a fresh short timeout, fail-closed
	// immediately before minting (D-26). The job-start reauth inherited from
	// ExecuteJob cannot close the snapshot/verify time window.
	if strings.TrimSpace(job.Author) == "" {
		say("🔒 Push öncesi yetki doğrulanamadı — push yapılmadı.\n\nPush-time authorization could not be verified; nothing was pushed.")
		return e.failEditJob(ctx, job, "push-time reauthorization denied: empty author")
	}
	authCtx, authCancel := context.WithTimeout(ctx, 5*time.Second)
	allowed, aerr := gh.CanWriteRepository(authCtx, job.Owner, job.Repo, job.Author)
	authCancel()
	if aerr != nil || !allowed {
		log.Printf("[jobs] Edit push-time reauth denied for %s by %q: allowed=%v err=%v", job.ID, job.Author, allowed, aerr)
		say("🔒 Push öncesi yetki doğrulanamadı — push yapılmadı, lütfen yeniden isteyin.\n\nPush-time authorization could not be verified; nothing was pushed. Please re-request.")
		return e.failEditJob(ctx, job, "push-time reauthorization denied")
	}

	// 8) Close the verify→push race: re-read the live head, then CAS (D-31).
	livePR2, prErr2 := gh.GetPR(ctx, job.Owner, job.Repo, job.PRNumber)
	if prErr2 != nil {
		log.Printf("[jobs] Edit pre-push head fetch failed for %s: %v", job.ID, prErr2)
		say(fmt.Sprintf("❌ Push öncesi head doğrulanamadı — push yapılmadı: %v\n\nPre-push head verification failed; nothing was pushed: %v", prErr2, prErr2))
		return e.failEditJob(ctx, job, prErr2.Error())
	}
	if err := ghclient.ValidateCommitOID(livePR2.HeadSHA); err != nil {
		return e.failEditJob(ctx, job, fmt.Sprintf("invalid pre-push head SHA %s: %v", livePR2.HeadSHA, err))
	}
	if livePR2.HeadSHA != job.HeadSHA {
		say(fmt.Sprintf("⏭️ %s commit'i geride kaldı (canlı: %s) — push yapılmadı, lütfen yeniden isteyin.\n\nHead moved during the edit (job head %s, live %s); nothing was pushed. Please re-request with a fresh `/improve --commit` or `@pr-review`.", short8(job.HeadSHA), short8(livePR2.HeadSHA), job.HeadSHA, livePR2.HeadSHA))
		return e.supersedeEditJob(ctx, job, fmt.Sprintf("stale head: job %s, live %s", job.HeadSHA, livePR2.HeadSHA))
	}
	if strings.TrimSpace(livePR2.HeadRef) == "" {
		return e.failEditJob(ctx, job, "live PR head ref is empty")
	}
	if livePR2.HeadRepoID <= 0 {
		say("🔒 Head depo kimliği doğrulanamadı — push yapılmadı.\n\nHead repository identity could not be verified; nothing was pushed.")
		return e.failEditJob(ctx, job, "head repository ID unavailable")
	}
	cred, cerr := gh.CreatePushCredential(ctx, job.Owner, job.Repo, livePR2.HeadRepoID)
	if cerr != nil {
		log.Printf("[jobs] Edit push credential mint failed for %s: %v", job.ID, cerr)
		say(fmt.Sprintf("🔒 Push kimlik bilgisi alınamadı — push yapılmadı: %v\n\nCould not mint the push credential; nothing was pushed: %v", cerr, cerr))
		return e.failEditJob(ctx, job, cerr.Error())
	}
	defer func() {
		if rerr := gh.RevokePushCredential(ctx, cred); rerr != nil {
			log.Printf("[jobs] Edit push credential revoke for %s failed: %v", job.ID, rerr)
		}
	}()
	headline := ghclient.SanitizeCommitHeadline(englishSummary, job.Payload, shortCommitSHA(job.HeadSHA))
	newSHA, perr := gh.CommitPush(ctx, cred, job.Owner, job.Repo, livePR2.HeadRef, job.HeadSHA, headline, files, nil)
	if perr != nil {
		var hme *ghclient.HeadMovedError
		switch {
		case errors.As(perr, &hme):
			actual := hme.ActualHeadOID
			if actual == "" {
				actual = "unknown"
			}
			say(fmt.Sprintf("⏭️ %s commit'i push sırasında geride kaldı (canlı: %s) — push yapılmadı, lütfen yeniden isteyin.\n\nHead moved during push (expected %s, live %s); nothing was pushed. Please re-request with a fresh `/improve --commit` or `@pr-review`.", short8(job.HeadSHA), short8(actual), job.HeadSHA, actual))
			return e.supersedeEditJob(ctx, job, perr.Error())
		case ghclient.IsUncertainWriteError(perr):
			e.markNeedsAttention(ctx, job, fmt.Sprintf("uncertain push outcome; requires operator resolution: %v", perr))
			return perr
		case isPushRefusedError(perr):
			say(fmt.Sprintf("🔒 Push reddedildi — push yapılmadı: %v\n\nPush refused; nothing was pushed: %v", perr, perr))
			return e.failEditJob(ctx, job, perr.Error())
		default:
			say(fmt.Sprintf("❌ Push başarısız — push yapılmadı: %v\n\nPush failed; nothing was pushed: %v", perr, perr))
			return e.failEditJob(ctx, job, perr.Error())
		}
	}

	// 9) Sticky persistence plus the final summary. The push already landed,
	// so a persistence failure is logged but never flips the outcome.
	if st, gerr := e.store.GetPRState(ctx, job.PRKey); gerr == nil && st != nil {
		st.LastBotCommitSHA = newSHA
		st.LastBotBaseSHA = job.HeadSHA
		st.EditHistory = appendEditHistory(st.EditHistory, job.Payload)
		if uerr := e.store.UpdatePRState(ctx, st); uerr != nil {
			log.Printf("[jobs] Edit sticky persist for %s failed: %v", job.ID, uerr)
		}
	} else if errors.Is(gerr, ErrPRNotFound) {
		fresh := &PRState{
			PRKey:            job.PRKey,
			Owner:            job.Owner,
			Repo:             job.Repo,
			Number:           job.PRNumber,
			LastBotCommitSHA: newSHA,
			LastBotBaseSHA:   job.HeadSHA,
			EditHistory:      []string{job.Payload},
		}
		if uerr := e.store.UpdatePRState(ctx, fresh); uerr != nil {
			log.Printf("[jobs] Edit sticky persist for %s failed: %v", job.ID, uerr)
		}
	} else {
		log.Printf("[jobs] Edit sticky read for %s failed: %v", job.ID, gerr)
	}
	quoted := make([]string, 0, len(editResult.FilesChanged))
	for _, p := range editResult.FilesChanged {
		quoted = append(quoted, "`"+p+"`")
	}
	summary := fmt.Sprintf("✅ Düzenleme push edildi: %s\n\nFiles changed (%d): %s\nVerification: passed\nCommit: %s",
		newSHA, len(editResult.FilesChanged), strings.Join(quoted, ", "), newSHA)
	if editResult.Truncated && editResult.OmissionNotice != "" {
		summary += fmt.Sprintf("\n\nPartial: %s", editResult.OmissionNotice)
	}
	say(summary)
	return e.completeEditJob(ctx, job)
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
