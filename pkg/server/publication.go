package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

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

// PublishStatus updates or creates a service-owned status comment for a job.
// It patches existing status comments to prevent comment spam per transition,
// and only creates a replacement if a complete bounded scan proves absence.
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

	// 1. If status comment ID is already known, update it
	if job.StatusCommentID > 0 {
		err := p.gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, bodyWithMarker)
		if err == nil {
			return job.StatusCommentID, nil
		}

		// Check if 404 (deleted comment)
		var ghErr interface{ StatusCode() int }
		is404 := false
		if errors.As(err, &ghErr) && ghErr.StatusCode() == 404 {
			is404 = true
		} else if strings.Contains(err.Error(), "404") {
			is404 = true
		}

		if !is404 {
			return 0, err
		}

		// Comment was deleted externally. Only replace after a complete absence check.
		actor, actErr := p.gh.ServiceActor(ctx, job.Owner, job.Repo)
		if actErr != nil {
			return 0, fmt.Errorf("service actor lookup failed during 404 absence check: %w", actErr)
		}

		match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
			Owner:    job.Owner,
			Repo:     job.Repo,
			PRNumber: job.PRNumber,
			Marker:   marker,
			Actor:    actor,
		})
		if scanErr != nil || match.Uncertain {
			return 0, fmt.Errorf("uncertain scan after status comment 404: %v", scanErr)
		}

		if match.Found {
			// Found under another ID; reuse it
			job.StatusCommentID = match.CommentID
			_ = p.gh.EditComment(ctx, job.Owner, job.Repo, job.StatusCommentID, bodyWithMarker)
			_ = p.store.UpdateJob(ctx, job)
			return match.CommentID, nil
		}

		// Confirmed absent: create replacement comment
		newID, createErr := p.gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, bodyWithMarker)
		if createErr != nil {
			return 0, createErr
		}
		job.StatusCommentID = newID
		_ = p.store.UpdateJob(ctx, job)
		return newID, nil
	}

	// 2. Status comment ID is 0. Check if an owned status comment already exists
	actor, actErr := p.gh.ServiceActor(ctx, job.Owner, job.Repo)
	if actErr == nil && actor != nil {
		match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
			Owner:    job.Owner,
			Repo:     job.Repo,
			PRNumber: job.PRNumber,
			Marker:   marker,
			Actor:    actor,
		})
		if scanErr == nil && match.Found {
			job.StatusCommentID = match.CommentID
			_ = p.gh.EditComment(ctx, job.Owner, job.Repo, match.CommentID, bodyWithMarker)
			_ = p.store.UpdateJob(ctx, job)
			return match.CommentID, nil
		}
		if match != nil && match.Uncertain {
			return 0, fmt.Errorf("uncertain scan for initial status comment: %s", match.Reason)
		}
	}

	// 3. Create initial status comment and save intent
	statusIntent := &OutputIntent{
		Marker:    marker,
		JobID:     job.ID,
		Action:    "status",
		PRKey:     job.PRKey,
		Owner:     job.Owner,
		Repo:      job.Repo,
		PRNumber:  job.PRNumber,
		ExactHead: job.HeadSHA,
		Body:      statusText,
		Status:    "pending",
	}
	_ = p.store.SaveOutputIntent(ctx, statusIntent)

	newID, createErr := p.gh.CreateComment(ctx, job.Owner, job.Repo, job.PRNumber, bodyWithMarker)
	if createErr != nil {
		statusIntent.Status = "uncertain"
		_ = p.store.UpdateOutputIntent(ctx, statusIntent)
		return 0, createErr
	}

	statusIntent.CommentID = newID
	statusIntent.Status = "in_progress"
	_ = p.store.UpdateOutputIntent(ctx, statusIntent)

	job.StatusCommentID = newID
	_ = p.store.UpdateJob(ctx, job)
	return newID, nil
}

// ReconcileResult describes the resolution of an OutputIntent reconciliation.
type ReconcileResult struct {
	Status    string // "completed", "superseded", "uncertain", "needs_attention"
	CommentID int64
	Reused    bool
	Error     error
}

// ReconcileOutput reconciles an OutputIntent across every crash window:
// 1. Crash before send: scans comments, verifies absent, checks live head, sends comment, saves ID.
// 2. Crash after remote commit before ID saved: scans comments, recovers matching service-owned comment, saves ID, avoids duplicates.
// 3. Crash after ID saved before terminal transition: verifies comment, commits terminal status atomically.
// 4. In-flight head movement: marks superseded without relabelling, schedules successor review.
// 5. API errors or incomplete scans: transitions to "uncertain" without retrying in a paid loop.
func (p *Publication) ReconcileOutput(ctx context.Context, intent *OutputIntent) (*ReconcileResult, error) {
	if intent == nil {
		return nil, errors.New("nil output intent")
	}
	if p.gh == nil {
		return &ReconcileResult{Status: "uncertain", Error: errors.New("github client is not initialized")}, nil
	}

	job, err := p.store.GetJob(ctx, intent.JobID)
	if err != nil || job == nil {
		return &ReconcileResult{Status: "uncertain", Error: fmt.Errorf("job not found for intent %s: %w", intent.JobID, err)}, nil
	}

	// 1. Verify service actor identity
	actor, err := p.gh.ServiceActor(ctx, intent.Owner, intent.Repo)
	if err != nil {
		intent.Status = "uncertain"
		_ = p.store.UpdateOutputIntent(ctx, intent)
		job.Status = "uncertain"
		job.Error = fmt.Sprintf("service actor lookup failed: %v", err)
		_ = p.store.UpdateJob(ctx, job)
		return &ReconcileResult{Status: "uncertain", Error: err}, nil
	}

	// Calculate body digest if missing
	if intent.BodyDigest == "" && intent.Body != "" {
		h := sha256.Sum256([]byte(intent.Body))
		intent.BodyDigest = hex.EncodeToString(h[:])
	}

	reused := false

	// 2. Check if comment already exists remotely
	if intent.CommentID > 0 {
		comment, err := p.gh.GetComment(ctx, intent.Owner, intent.Repo, intent.CommentID)
		if err == nil && comment != nil {
			// Verify author matches verified service actor
			user := comment.GetUser()
			actorMatches := false
			if actor.ID > 0 && user.GetID() == actor.ID {
				actorMatches = true
			} else if actor.Login != "" && strings.EqualFold(user.GetLogin(), actor.Login) {
				actorMatches = true
			}

			if actorMatches && strings.Contains(comment.GetBody(), intent.Marker) {
				reused = true
			} else {
				// Foreign or mismatched comment at this ID; reset CommentID
				intent.CommentID = 0
			}
		} else {
			// GetComment failed. Check if 404
			is404 := false
			var ghErr interface{ StatusCode() int }
			if errors.As(err, &ghErr) && ghErr.StatusCode() == 404 {
				is404 = true
			} else if strings.Contains(err.Error(), "404") {
				is404 = true
			}

			if is404 {
				// Comment deleted. Perform complete scan before replacement.
				match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
					Owner:      intent.Owner,
					Repo:       intent.Repo,
					PRNumber:   intent.PRNumber,
					Marker:     intent.Marker,
					BodyDigest: intent.BodyDigest,
					Actor:      actor,
				})
				if scanErr != nil || match.Uncertain {
					intent.Status = "uncertain"
					_ = p.store.UpdateOutputIntent(ctx, intent)
					job.Status = "uncertain"
					_ = p.store.UpdateJob(ctx, job)
					return &ReconcileResult{Status: "uncertain", Error: fmt.Errorf("uncertain scan after 404: %v", scanErr)}, nil
				}
				if match.Found {
					intent.CommentID = match.CommentID
					reused = true
				} else {
					intent.CommentID = 0
				}
			} else {
				// Transient network or server error during GetComment yields uncertain
				intent.Status = "uncertain"
				_ = p.store.UpdateOutputIntent(ctx, intent)
				job.Status = "uncertain"
				job.Error = err.Error()
				_ = p.store.UpdateJob(ctx, job)
				return &ReconcileResult{Status: "uncertain", Error: err}, nil
			}
		}
	}

	if intent.CommentID == 0 {
		// Scan for existing owned comment (crash after remote commit, before ID saved locally)
		match, scanErr := p.gh.FindOwnedComment(ctx, ghclient.FindOwnedCommentOptions{
			Owner:      intent.Owner,
			Repo:       intent.Repo,
			PRNumber:   intent.PRNumber,
			Marker:     intent.Marker,
			BodyDigest: intent.BodyDigest,
			Actor:      actor,
		})
		if scanErr != nil || match.Uncertain {
			intent.Status = "uncertain"
			_ = p.store.UpdateOutputIntent(ctx, intent)
			job.Status = "uncertain"
			job.Error = fmt.Sprintf("uncertain owned comment scan: %v", scanErr)
			_ = p.store.UpdateJob(ctx, job)
			return &ReconcileResult{Status: "uncertain", Error: scanErr}, nil
		}

		if match.Found {
			// Recovered matching comment!
			intent.CommentID = match.CommentID
			_ = p.store.UpdateOutputIntent(ctx, intent)
			reused = true
		}
	}

	// 3. If comment is still not posted, perform pre-write check and write
	if intent.CommentID == 0 {
		// Pre-write check: live PR head and generation
		livePR, prErr := p.gh.GetPR(ctx, intent.Owner, intent.Repo, intent.PRNumber)
		if prErr != nil {
			intent.Status = "uncertain"
			_ = p.store.UpdateOutputIntent(ctx, intent)
			job.Status = "uncertain"
			job.Error = prErr.Error()
			_ = p.store.UpdateJob(ctx, job)
			return &ReconcileResult{Status: "uncertain", Error: prErr}, nil
		}

		prState, _ := p.store.GetPRState(ctx, intent.PRKey)
		genChanged := prState != nil && prState.Generation > job.Generation
		headChanged := livePR.HeadSHA != intent.ExactHead

		if genChanged || headChanged {
			// Discard outdated report; do not post stale analysis
			intent.Status = "superseded"
			job.Status = "superseded"
			now := time.Now().UTC()
			job.FinishedAt = &now
			_ = p.store.UpdateOutputIntent(ctx, intent)
			_ = p.store.UpdateJob(ctx, job)

			if headChanged && job.Trigger == "automatic" {
				_, _ = p.store.ScheduleSuccessorReview(ctx, intent.PRKey, intent.Owner, intent.Repo, intent.PRNumber, livePR.BaseSHA, livePR.HeadSHA)
			}
			return &ReconcileResult{Status: "superseded"}, nil
		}

		bodyWithMarker := intent.Body
		if !strings.Contains(bodyWithMarker, intent.Marker) {
			bodyWithMarker = fmt.Sprintf("%s\n\n%s", intent.Body, intent.Marker)
		}

		newID, createErr := p.gh.CreateComment(ctx, intent.Owner, intent.Repo, intent.PRNumber, bodyWithMarker)
		if createErr != nil {
			intent.Status = "uncertain"
			_ = p.store.UpdateOutputIntent(ctx, intent)
			job.Status = "uncertain"
			job.Error = createErr.Error()
			_ = p.store.UpdateJob(ctx, job)
			return &ReconcileResult{Status: "uncertain", Error: createErr}, nil
		}

		intent.CommentID = newID
		_ = p.store.UpdateOutputIntent(ctx, intent)
	}

	// 4. Post-write head check (detect head moving in-flight)
	livePR2, prErr2 := p.gh.GetPR(ctx, intent.Owner, intent.Repo, intent.PRNumber)
	if prErr2 == nil && livePR2 != nil && livePR2.HeadSHA != intent.ExactHead {
		// Commit head moved while posting; mark superseded
		if intent.CommentID > 0 {
			_ = p.gh.EditComment(ctx, intent.Owner, intent.Repo, intent.CommentID,
				fmt.Sprintf("%s\n\n*(Note: Commit head moved during review publication. This review is superseded.)*", intent.Body))
		}
		intent.Status = "superseded"
		job.Status = "superseded"
		now := time.Now().UTC()
		job.FinishedAt = &now
		_ = p.store.UpdateOutputIntent(ctx, intent)
		_ = p.store.UpdateJob(ctx, job)

		if job.Trigger == "automatic" {
			_, _ = p.store.ScheduleSuccessorReview(ctx, intent.PRKey, intent.Owner, intent.Repo, intent.PRNumber, livePR2.BaseSHA, livePR2.HeadSHA)
		}
		return &ReconcileResult{Status: "superseded", CommentID: intent.CommentID, Reused: reused}, nil
	}

	// 5. Terminal transition
	intent.Status = "completed"
	job.Status = "completed"
	now := time.Now().UTC()
	job.FinishedAt = &now
	_ = p.store.UpdateOutputIntent(ctx, intent)
	_ = p.store.UpdateJob(ctx, job)

	if intent.Action == "review_output" {
		prState, _ := p.store.GetPRState(ctx, intent.PRKey)
		if prState != nil {
			prState.LastReviewedHead = intent.ExactHead
			_ = p.store.UpdatePRState(ctx, prState)
		}
	}

	return &ReconcileResult{Status: "completed", CommentID: intent.CommentID, Reused: reused}, nil
}
