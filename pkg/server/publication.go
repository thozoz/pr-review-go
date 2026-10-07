package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
)

// Publication handles safe, idempotent, durable status and review publication.
// It ensures that status comments are reused across transitions, review outputs
// are persisted before external writes, and ambiguous writes are safely reconciled
// without duplicating comments or repeating expensive LLM review generation.
type Publication struct {
	store JobStore
	gh    *ghclient.Client
}

// NewPublication creates a new Publication reconciler.
func NewPublication(store JobStore, gh *ghclient.Client) *Publication {
	return &Publication{
		store: store,
		gh:    gh,
	}
}

// ErrPublicationPersistence marks failures of the durable ledger (saving or
// committing intent/job/outcome state). Callers must treat these as non-success:
// accepted work is preserved and never reported completed.
var ErrPublicationPersistence = errors.New("durable publication state unavailable")

func persistErr(what string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrPublicationPersistence, what, err)
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var ghErr interface{ StatusCode() int }
	if errors.As(err, &ghErr) && ghErr.StatusCode() == 404 {
		return true
	}
	var rErr *github.ErrorResponse
	if errors.As(err, &rErr) && rErr.Response != nil && rErr.Response.StatusCode == 404 {
		return true
	}
	return strings.Contains(err.Error(), "404")
}

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func actorOwns(actor *ghclient.ServiceActor, c *github.IssueComment) bool {
	if actor == nil || c == nil {
		return false
	}
	user := c.GetUser()
	if user == nil {
		return false
	}
	if actor.ID > 0 && user.GetID() == actor.ID {
		return true
	}
	return actor.Login != "" && strings.EqualFold(user.GetLogin(), actor.Login)
}

// ownedByID proves, via GetComment, that comment id is authored by the service
// actor and carries the marker. notFound reports a 404; any other failure is err.
func (p *Publication) ownedByID(ctx context.Context, owner, repo string, id int64, marker string, actor *ghclient.ServiceActor) (owned, notFound bool, body string, err error) {
	if id <= 0 {
		return false, false, "", fmt.Errorf("invalid comment id %d", id)
	}
	c, gerr := p.gh.GetComment(ctx, owner, repo, id)
	if gerr != nil {
		if isNotFoundErr(gerr) {
			return false, true, "", nil
		}
		return false, false, "", gerr
	}
	if c == nil {
		return false, false, "", errors.New("github returned nil comment")
	}
	if !actorOwns(actor, c) || !strings.Contains(c.GetBody(), marker) {
		return false, false, c.GetBody(), nil
	}
	return true, false, c.GetBody(), nil
}

// PublishStatus updates or creates a service-owned status comment for a job.
// Every create/edit/replacement first commits a recoverable intent (body, stable
// marker, exact head, digest, owner identity); if that commit fails no remote write
// occurs. It patches existing owned status comments to prevent comment spam per
// transition, and only creates a replacement if a complete bounded scan proves absence.
func (p *Publication) PublishStatus(ctx context.Context, job *Job, statusText string) (int64, error) {
	if job == nil {
		return 0, errors.New("nil job provided")
	}
	if p.gh == nil {
		return 0, errors.New("github client is not initialized")
	}

	marker := fmt.Sprintf("<!-- pr-review-status:%s -->", job.ID)
	bodyWithMarker := statusText
	if !strings.Contains(bodyWithMarker, marker) {
		bodyWithMarker = fmt.Sprintf("%s\n\n%s", statusText, marker)
	}

	// Failed actor lookup never authorizes a write.
	actor, actErr := p.gh.ServiceActor(ctx, job.Owner, job.Repo)
	if actErr != nil || actor == nil {
		return 0, fmt.Errorf("service actor lookup failed; status write not authorized: %v", actErr)
	}

	intent := &OutputIntent{
		Marker:        marker,
		JobID:         job.ID,
		Action:        "status",
		PRKey:         job.PRKey,
		Owner:         job.Owner,
		Repo:          job.Repo,
		PRNumber:      job.PRNumber,
		ExactHead:     job.HeadSHA,
		Body:          bodyWithMarker,
		BodyDigest:    digestOf(bodyWithMarker),
		VerifiedActor: actor.Login,
		Status:        "pending",
	}
	saveIntent := func() error {
		if err := p.store.SaveOutputIntent(ctx, intent); err != nil {
			return persistErr("save status intent", err)
		}
		return nil
	}
	finish := func(id int64) (int64, error) {
		intent.CommentID = id
		intent.Status = "completed"
		if err := p.store.UpdateOutputIntent(ctx, intent); err != nil {
			return 0, persistErr("commit status intent", err)
		}
		return id, nil
	}
	markUncertain := func() {
		intent.Status = "uncertain"
		_ = p.store.UpdateOutputIntent(ctx, intent)
	}
	// editOwned edits a comment already proven owned.
	editOwned := func(id int64) (int64, error) {
		intent.CommentID = id
		if err := saveIntent(); err != nil {
			return 0, err
		}
		if err := p.gh.EditComment(ctx, job.Owner, job.Repo, id, bodyWithMarker); err != nil {
			markUncertain()
			return 0, err
		}
		if job.StatusCommentID != id {
			job.StatusCommentID = id
			if err := p.store.UpdateJob(ctx, job); err != nil {
				return 0, persistErr("save status comment id", err)
			}
		}
		return finish(id)
	}

	// 1. Known ID: edit only after owned-comment proof.
	if job.StatusCommentID > 0 {
		owned, notFound, _, err := p.ownedByID(ctx, job.Owner, job.Repo, job.StatusCommentID, marker, actor)
		if err != nil {
			return 0, fmt.Errorf("status comment ownership check failed: %w", err)
		}
		if owned {
			return editOwned(job.StatusCommentID)
		}
		_ = notFound // deleted or foreign: fall through to a complete absence scan
	}

	// 2. Complete bounded scan for an owned status comment.
	match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
		Owner:    job.Owner,
		Repo:     job.Repo,
		PRNumber: job.PRNumber,
		Marker:   marker,
		Actor:    actor,
	})
	if scanErr != nil || match == nil || match.Uncertain {
		reason := ""
		if match != nil {
			reason = match.Reason
		}
		return 0, fmt.Errorf("uncertain status comment scan (no write authorized): %v %s", scanErr, reason)
	}
	if match.Found {
		if match.CommentID <= 0 {
			return 0, fmt.Errorf("scan returned invalid comment id %d", match.CommentID)
		}
		return editOwned(match.CommentID)
	}

	// 3. Confirmed absent: commit intent, then create.
	if err := saveIntent(); err != nil {
		return 0, err
	}
	newID, createErr := p.gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, bodyWithMarker)
	if createErr != nil {
		markUncertain()
		return 0, createErr
	}
	intent.CommentID = newID
	intent.Status = "in_progress"
	if err := p.store.UpdateOutputIntent(ctx, intent); err != nil {
		return 0, persistErr("save created status comment id", err)
	}
	job.StatusCommentID = newID
	if err := p.store.UpdateJob(ctx, job); err != nil {
		return 0, persistErr("save status comment id on job", err)
	}
	return finish(newID)
}

// ReconcileResult describes the resolution of an OutputIntent reconciliation.
type ReconcileResult struct {
	Status    string // "completed", "superseded", "uncertain", "needs_attention"
	CommentID int64
	Reused    bool
	Error     error
}

// ReconcileOutput reconciles a committed OutputIntent across every crash window.
// It never trusts the caller's in-memory intent alone: the saved record must exist,
// be readable and match. Any failed/nil/malformed final PR head, failed ownership
// proof or unavailable generation/ledger state yields an uncertain, nonterminal
// outcome that retains the original body, digest, marker and comment ID.
func (p *Publication) ReconcileOutput(ctx context.Context, intent *OutputIntent) (*ReconcileResult, error) {
	if intent == nil {
		return nil, errors.New("nil output intent")
	}
	if p.gh == nil {
		return &ReconcileResult{Status: "uncertain", Error: errors.New("github client is not initialized")}, nil
	}

	// 0. Require a committed, readable, matching saved intent.
	saved, err := p.store.GetOutputIntent(ctx, intent.Marker)
	if err != nil || saved == nil {
		return &ReconcileResult{Status: "uncertain", Error: fmt.Errorf("no committed intent for %s: %w", intent.Marker, err)}, nil
	}
	if saved.JobID != intent.JobID || saved.ExactHead != intent.ExactHead || saved.Action != intent.Action ||
		saved.Marker != intent.Marker || digestOf(saved.Body) != digestOf(intent.Body) {
		return &ReconcileResult{Status: "uncertain", Error: fmt.Errorf("saved intent for %s does not match caller intent", intent.Marker)}, nil
	}
	if intent.CommentID == 0 {
		intent.CommentID = saved.CommentID
	}
	if intent.VerifiedActor == "" {
		intent.VerifiedActor = saved.VerifiedActor
	}
	intent.SupersededBody = saved.SupersededBody
	intent.SupersededDigest = saved.SupersededDigest
	if intent.BodyDigest == "" {
		intent.BodyDigest = digestOf(intent.Body)
	}

	job, err := p.store.GetJob(ctx, intent.JobID)
	if err != nil || job == nil {
		return &ReconcileResult{Status: "uncertain", Error: fmt.Errorf("job not found for intent %s: %v", intent.JobID, err)}, nil
	}

	bodyWithMarker := intent.Body
	if !strings.Contains(bodyWithMarker, intent.Marker) {
		bodyWithMarker = fmt.Sprintf("%s\n\n%s", intent.Body, intent.Marker)
	}
	// Owned content that restart may legitimately find: original or saved superseded body.
	allowedDigests := map[string]bool{
		digestOf(bodyWithMarker): true,
		digestOf(intent.Body):    true,
	}
	if intent.SupersededDigest != "" {
		allowedDigests[intent.SupersededDigest] = true
	}

	// uncertain retains saved output/identity; job becomes nonterminal "uncertain".
	uncertain := func(cause error) (*ReconcileResult, error) {
		intent.Status = "uncertain"
		job.Status = "uncertain"
		job.Error = cause.Error()
		job.FinishedAt = nil
		// Atomic best-effort; failure cannot make it completed.
		_ = p.store.CommitPublicationOutcome(ctx, intent, job, nil)
		return &ReconcileResult{Status: "uncertain", CommentID: intent.CommentID, Error: cause}, nil
	}

	// 1. Verify service actor identity.
	actor, err := p.gh.ServiceActor(ctx, intent.Owner, intent.Repo)
	if err != nil || actor == nil {
		return uncertain(fmt.Errorf("service actor lookup failed: %v", err))
	}

	reused := false

	// 2. Known comment ID: require owned-comment proof.
	if intent.CommentID > 0 {
		owned, notFound, body, oerr := p.ownedByID(ctx, intent.Owner, intent.Repo, intent.CommentID, intent.Marker, actor)
		switch {
		case oerr != nil:
			return uncertain(fmt.Errorf("owned comment check failed: %w", oerr))
		case owned && !allowedDigests[digestOf(body)]:
			return uncertain(errors.New("owned comment body does not match saved intent; refusing to adopt"))
		case owned:
			reused = true
		default:
			_ = notFound // deleted or foreign: fall through to complete scan
			intent.CommentID = 0
		}
	}

	if intent.CommentID == 0 {
		match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
			Owner:    intent.Owner,
			Repo:     intent.Repo,
			PRNumber: intent.PRNumber,
			Marker:   intent.Marker,
			Actor:    actor,
		})
		if scanErr != nil || match == nil || match.Uncertain {
			return uncertain(fmt.Errorf("uncertain owned comment scan: %v", scanErr))
		}
		if match.Found {
			if match.CommentID <= 0 {
				return uncertain(fmt.Errorf("scan returned invalid comment id %d", match.CommentID))
			}
			if !allowedDigests[digestOf(match.Body)] {
				return uncertain(errors.New("owned comment body does not match saved intent; refusing to adopt"))
			}
			intent.CommentID = match.CommentID
			if err := p.store.UpdateOutputIntent(ctx, intent); err != nil {
				return uncertain(persistErr("save recovered comment id", err))
			}
			reused = true
		}
	}

	// 3. Not posted: pre-write head/generation check, then write.
	if intent.CommentID == 0 {
		livePR, prErr := p.gh.GetPR(ctx, intent.Owner, intent.Repo, intent.PRNumber)
		if prErr != nil || livePR == nil || ghclient.ValidateCommitOID(livePR.HeadSHA) != nil {
			return uncertain(fmt.Errorf("pre-write head verification failed: %v", prErr))
		}
		prState, psErr := p.store.GetPRState(ctx, intent.PRKey)
		if psErr != nil && !errors.Is(psErr, ErrPRNotFound) {
			return uncertain(persistErr("read pr state", psErr))
		}
		genChanged := prState != nil && prState.Generation > job.Generation
		headChanged := livePR.HeadSHA != intent.ExactHead

		if genChanged || headChanged {
			// Nothing was posted; discard outdated report, never post stale analysis.
			intent.Status = "superseded"
			job.Status = "superseded"
			now := time.Now().UTC()
			job.FinishedAt = &now
			succBase, succHead := "", ""
			if headChanged && job.Trigger == "automatic" {
				succBase, succHead = livePR.BaseSHA, livePR.HeadSHA
			}
			if err := p.store.CommitSupersededOutcome(ctx, intent, job, succBase, succHead); err != nil {
				return uncertain(persistErr("commit superseded outcome", err))
			}
			return &ReconcileResult{Status: "superseded"}, nil
		}

		newID, createErr := p.gh.CreateComment(ctx, intent.Owner, intent.Repo, intent.PRNumber, bodyWithMarker)
		if createErr != nil {
			return uncertain(createErr)
		}
		intent.CommentID = newID
		intent.Status = "in_progress"
		if err := p.store.UpdateOutputIntent(ctx, intent); err != nil {
			// Pre-write record stays recoverable: marker scan finds the remote comment.
			intent.CommentID = 0
			return uncertain(persistErr("save created comment id", err))
		}
	}

	// 4. Final head and generation verification; any uncertainty is nonterminal.
	livePR2, prErr2 := p.gh.GetPR(ctx, intent.Owner, intent.Repo, intent.PRNumber)
	if prErr2 != nil || livePR2 == nil || ghclient.ValidateCommitOID(livePR2.HeadSHA) != nil {
		return uncertain(fmt.Errorf("final head verification failed: %v", prErr2))
	}
	prState2, psErr2 := p.store.GetPRState(ctx, intent.PRKey)
	if psErr2 != nil && !errors.Is(psErr2, ErrPRNotFound) {
		return uncertain(persistErr("read pr state for final commit", psErr2))
	}
	headMoved := livePR2.HeadSHA != intent.ExactHead
	genMoved := prState2 != nil && prState2.Generation > job.Generation

	if headMoved || genMoved {
		// Keep the original marker; persist the intended superseded body/digest first.
		if intent.SupersededBody == "" {
			intent.SupersededBody = fmt.Sprintf("%s\n\n*(Note: Commit head moved during review publication. This review is superseded.)*", bodyWithMarker)
			intent.SupersededDigest = digestOf(intent.SupersededBody)
		}
		intent.Status = "superseding"
		if err := p.store.UpdateOutputIntent(ctx, intent); err != nil {
			return uncertain(persistErr("save superseded body", err))
		}
		if err := p.gh.EditComment(ctx, intent.Owner, intent.Repo, intent.CommentID, intent.SupersededBody); err != nil {
			return uncertain(fmt.Errorf("superseded edit failed: %w", err))
		}
		intent.Status = "superseded"
		job.Status = "superseded"
		now := time.Now().UTC()
		job.FinishedAt = &now
		succBase, succHead := "", ""
		if job.Trigger == "automatic" {
			succBase, succHead = livePR2.BaseSHA, livePR2.HeadSHA
		}
		if err := p.store.CommitSupersededOutcome(ctx, intent, job, succBase, succHead); err != nil {
			return uncertain(persistErr("commit superseded outcome", err))
		}
		return &ReconcileResult{Status: "superseded", CommentID: intent.CommentID, Reused: reused}, nil
	}

	// 5. Confirmed current-head publication: atomic terminal commit.
	intent.Status = "completed"
	job.Status = "completed"
	job.Error = ""
	now := time.Now().UTC()
	job.FinishedAt = &now
	var outState *PRState
	if intent.Action == "review_output" {
		outState = &PRState{PRKey: intent.PRKey, LastReviewedHead: intent.ExactHead}
		if prState2 != nil {
			cp := *prState2
			cp.LastReviewedHead = intent.ExactHead
			outState = &cp
		}
	}
	if err := p.store.CommitPublicationOutcome(ctx, intent, job, outState); err != nil {
		return uncertain(fmt.Errorf("terminal outcome commit failed: %w", err))
	}
	return &ReconcileResult{Status: "completed", CommentID: intent.CommentID, Reused: reused}, nil
}
