package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"go.etcd.io/bbolt"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
)

var (
	// ErrDuplicateDecision indicates a decision for this PR and head commit has already been claimed or published.
	ErrDuplicateDecision = errors.New("decision already claimed or published for head")
	// ErrDecisionNotFound indicates that no decision record exists for the given PR and head commit.
	ErrDecisionNotFound = errors.New("decision record not found")
	// ErrLedgerClosed indicates that the underlying ledger database is closed or uninitialized.
	ErrLedgerClosed = errors.New("decision ledger database is closed or nil")
)

// DecisionStatus represents the lifecycle state of a decision claim.
type DecisionStatus string

const (
	// DecisionStatusClaimed indicates the decision has been reserved but remote publication is in progress.
	DecisionStatusClaimed DecisionStatus = "claimed"
	// DecisionStatusPublished indicates the decision was successfully created on GitHub.
	DecisionStatusPublished DecisionStatus = "published"
	// DecisionStatusUncertain indicates an ambiguous remote outcome occurred requiring operator resolution.
	DecisionStatusUncertain DecisionStatus = "uncertain"
)

// DecisionRecord records durable decision state for a specific PR and head commit.
// Key format in bbolt: "<Host>/<RepoID>/<PRNumber>/<HeadSHA>".
type DecisionRecord struct {
	PRKey       PRKey          `json:"pr_key"`
	HeadSHA     string         `json:"head_sha"`
	Event       string         `json:"event"`
	JobID       string         `json:"job_id"`
	ReviewID    int64          `json:"review_id,omitempty"`
	Status      DecisionStatus `json:"status"`
	ClaimedAt   time.Time      `json:"claimed_at"`
	PublishedAt *time.Time     `json:"published_at,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// DecisionLedger provides durable, atomic one-decision-per-head guarantees (D-04).
type DecisionLedger struct {
	db    *bbolt.DB
	locks sync.Map
}

// NewDecisionLedger creates a DecisionLedger backed by bbolt.
func NewDecisionLedger(db *bbolt.DB) *DecisionLedger {
	return &DecisionLedger{
		db: db,
	}
}

func decisionLedgerKey(prKey PRKey, headSHA string) []byte {
	return []byte(fmt.Sprintf("%s/%s", prKey.String(), headSHA))
}

func (l *DecisionLedger) lockKey(key string) func() {
	val, _ := l.locks.LoadOrStore(key, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	return func() {
		mu.Unlock()
	}
}

// ClaimDecision reserves the decision for (PRKey, headSHA).
// Serialized per (PRKey, headSHA) mutex.
// If a record already exists, returns claimed=false, existing record, and nil.
// If no record exists, writes a new claimed record and returns claimed=true, new record, and nil (D-04).
func (l *DecisionLedger) ClaimDecision(ctx context.Context, prKey PRKey, headSHA string, event string, jobID string) (bool, *DecisionRecord, error) {
	if l == nil || l.db == nil {
		return false, nil, ErrLedgerClosed
	}

	prKey.Host = strings.TrimSpace(prKey.Host)
	if prKey.Host == "" || prKey.RepoID <= 0 || prKey.Number <= 0 {
		return false, nil, fmt.Errorf("invalid PRKey: %v", prKey)
	}

	if err := ghclient.ValidateCommitOID(headSHA); err != nil {
		return false, nil, fmt.Errorf("invalid head SHA: %w", err)
	}

	eventUpper := strings.ToUpper(strings.TrimSpace(event))
	if eventUpper != ghclient.EventApprove && eventUpper != ghclient.EventRequestChanges {
		return false, nil, fmt.Errorf("%w: got %q", ghclient.ErrInvalidDecisionEvent, event)
	}

	keyStr := fmt.Sprintf("%s/%s", prKey.String(), headSHA)
	unlock := l.lockKey(keyStr)
	defer unlock()

	keyBytes := []byte(keyStr)
	var claimed bool
	var record *DecisionRecord

	err := l.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		if b == nil {
			var err error
			b, err = tx.CreateBucketIfNotExists(bucketDecisions)
			if err != nil {
				return err
			}
		}

		existing := b.Get(keyBytes)
		if existing != nil {
			var existingRec DecisionRecord
			if err := json.Unmarshal(existing, &existingRec); err != nil {
				return fmt.Errorf("corrupt decision record for %s: %w", keyStr, err)
			}
			claimed = false
			record = &existingRec
			return nil
		}

		newRec := &DecisionRecord{
			PRKey:     prKey,
			HeadSHA:   headSHA,
			Event:     eventUpper,
			JobID:     jobID,
			Status:    DecisionStatusClaimed,
			ClaimedAt: time.Now().UTC(),
		}
		raw, err := json.Marshal(newRec)
		if err != nil {
			return fmt.Errorf("failed marshaling decision record: %w", err)
		}

		if err := b.Put(keyBytes, raw); err != nil {
			return err
		}

		claimed = true
		record = newRec
		return nil
	})

	if err != nil {
		return false, nil, err
	}
	return claimed, record, nil
}

// RecordPublished marks a decision claim as successfully published with review ID.
func (l *DecisionLedger) RecordPublished(ctx context.Context, prKey PRKey, headSHA string, reviewID int64) error {
	if l == nil || l.db == nil {
		return ErrLedgerClosed
	}

	keyStr := fmt.Sprintf("%s/%s", prKey.String(), headSHA)
	unlock := l.lockKey(keyStr)
	defer unlock()

	keyBytes := []byte(keyStr)
	return l.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		if b == nil {
			return ErrDecisionNotFound
		}
		existing := b.Get(keyBytes)
		if existing == nil {
			return ErrDecisionNotFound
		}

		var rec DecisionRecord
		if err := json.Unmarshal(existing, &rec); err != nil {
			return fmt.Errorf("corrupt decision record: %w", err)
		}

		now := time.Now().UTC()
		rec.Status = DecisionStatusPublished
		rec.ReviewID = reviewID
		rec.PublishedAt = &now
		rec.Error = ""

		raw, err := json.Marshal(&rec)
		if err != nil {
			return err
		}
		return b.Put(keyBytes, raw)
	})
}

// MarkUncertain records an ambiguous remote write error for operator resolution.
func (l *DecisionLedger) MarkUncertain(ctx context.Context, prKey PRKey, headSHA string, reason string) error {
	if l == nil || l.db == nil {
		return ErrLedgerClosed
	}

	keyStr := fmt.Sprintf("%s/%s", prKey.String(), headSHA)
	unlock := l.lockKey(keyStr)
	defer unlock()

	keyBytes := []byte(keyStr)
	return l.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		if b == nil {
			return ErrDecisionNotFound
		}
		existing := b.Get(keyBytes)
		if existing == nil {
			return ErrDecisionNotFound
		}

		var rec DecisionRecord
		if err := json.Unmarshal(existing, &rec); err != nil {
			return fmt.Errorf("corrupt decision record: %w", err)
		}

		rec.Status = DecisionStatusUncertain
		rec.Error = reason

		raw, err := json.Marshal(&rec)
		if err != nil {
			return err
		}
		return b.Put(keyBytes, raw)
	})
}

// GetDecision returns the recorded decision for (PRKey, headSHA), or nil if none exists.
func (l *DecisionLedger) GetDecision(ctx context.Context, prKey PRKey, headSHA string) (*DecisionRecord, error) {
	if l == nil || l.db == nil {
		return nil, ErrLedgerClosed
	}

	if err := ghclient.ValidateCommitOID(headSHA); err != nil {
		return nil, fmt.Errorf("invalid head SHA: %w", err)
	}

	keyBytes := decisionLedgerKey(prKey, headSHA)
	var rec *DecisionRecord

	err := l.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		if b == nil {
			return nil
		}
		v := b.Get(keyBytes)
		if v == nil {
			return nil
		}
		var parsed DecisionRecord
		if err := json.Unmarshal(v, &parsed); err != nil {
			return fmt.Errorf("corrupt decision record: %w", err)
		}
		rec = &parsed
		return nil
	})

	return rec, err
}

// PruneTerminalDecisions removes expired published decisions older than cutoff.
// Part of terminal record maintenance; leaves claimed and uncertain decisions pinned.
func (l *DecisionLedger) PruneTerminalDecisions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if l == nil || l.db == nil {
		return 0, ErrLedgerClosed
	}
	if limit <= 0 {
		limit = 128
	}

	deleted := 0
	err := l.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		var toDelete [][]byte
		for k, v := c.First(); k != nil && len(toDelete) < limit; k, v = c.Next() {
			var rec DecisionRecord
			if err := json.Unmarshal(v, &rec); err == nil {
				// Only published records older than cutoff are pruned. Claimed and uncertain are pinned.
				if rec.Status == DecisionStatusPublished && rec.PublishedAt != nil && rec.PublishedAt.Before(cutoff) {
					toDelete = append(toDelete, append([]byte(nil), k...))
				}
			}
		}
		for _, k := range toDelete {
			if err := b.Delete(k); err == nil {
				deleted++
			}
		}
		return nil
	})

	return deleted, err
}

// DecisionReviewClient specifies the remote GitHub decision review write interface.
type DecisionReviewClient interface {
	PostDecisionReview(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []ghclient.InlineSuggestion) (*github.PullRequestReview, error)
}

// NeedsAttentionRecorder specifies the hook to flag a job as needs_attention.
type NeedsAttentionRecorder func(ctx context.Context, job *Job, reason string)

// PublishDecisionResult captures the outcome of PublishDecision.
type PublishDecisionResult struct {
	Duplicate bool
	Record    *DecisionRecord
	Review    *github.PullRequestReview
}

// PublishDecision performs the atomic check-then-publish decision lifecycle:
// 1. Claims decision in ledger (D-04). If already claimed/published, returns duplicate without network calls.
// 2. On claimed, executes exactly ONE remote PostDecisionReview (D-15).
// 3. On success, records published with review ID.
// 4. On uncertain write error (timeout / 5xx on POST), marks uncertain in ledger and routes to needs_attention (D-16).
func PublishDecision(
	ctx context.Context,
	ledger *DecisionLedger,
	gh DecisionReviewClient,
	job *Job,
	event string,
	body string,
	suggestions []ghclient.InlineSuggestion,
	markNeedsAttention NeedsAttentionRecorder,
) (*PublishDecisionResult, error) {
	if job == nil {
		return nil, errors.New("nil job provided")
	}
	if ledger == nil {
		return nil, errors.New("nil decision ledger provided")
	}
	if gh == nil {
		return nil, errors.New("nil decision review client provided")
	}

	claimed, rec, err := ledger.ClaimDecision(ctx, job.PRKey, job.HeadSHA, event, job.ID)
	if err != nil {
		return nil, fmt.Errorf("failed claiming decision in ledger: %w", err)
	}

	if !claimed {
		return &PublishDecisionResult{
			Duplicate: true,
			Record:    rec,
		}, ErrDuplicateDecision
	}

	review, err := gh.PostDecisionReview(ctx, job.Owner, job.Repo, job.PRNumber, job.HeadSHA, event, body, suggestions)
	if err != nil {
		if ghclient.IsUncertainWriteError(err) {
			_ = ledger.MarkUncertain(ctx, job.PRKey, job.HeadSHA, err.Error())
			if markNeedsAttention != nil {
				markNeedsAttention(ctx, job, fmt.Sprintf("uncertain decision write; requires operator resolution: %v", err))
			}
		}
		return nil, err
	}

	reviewID := int64(0)
	if review != nil {
		reviewID = review.GetID()
	}
	if rerr := ledger.RecordPublished(ctx, job.PRKey, job.HeadSHA, reviewID); rerr != nil {
		return nil, fmt.Errorf("failed recording published decision: %w", rerr)
	}

	return &PublishDecisionResult{
		Duplicate: false,
		Record:    rec,
		Review:    review,
	}, nil
}
