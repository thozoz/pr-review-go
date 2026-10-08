package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v68/github"
	"go.etcd.io/bbolt"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/retry"
)

type mockDecisionClient struct {
	mu           sync.Mutex
	calls        int
	postReviewFn func(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []ghclient.InlineSuggestion) (*github.PullRequestReview, error)
}

func (m *mockDecisionClient) PostDecisionReview(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []ghclient.InlineSuggestion) (*github.PullRequestReview, error) {
	m.mu.Lock()
	m.calls++
	fn := m.postReviewFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, owner, repo, number, commitSHA, event, body, suggestions)
	}
	return &github.PullRequestReview{
		ID: github.Ptr(int64(7001)),
	}, nil
}

func (m *mockDecisionClient) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func setupTestLedger(t *testing.T) (*BoltJobStore, *DecisionLedger, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_ledger.db")
	store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
	if err != nil {
		t.Fatalf("failed opening job store: %v", err)
	}

	ledger := NewDecisionLedger(store.DB())
	return store, ledger, dbPath
}

func TestDecisionLedger_SameHeadDuplicate(t *testing.T) {
	store, ledger, _ := setupTestLedger(t)
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 100, 1)
	headSHA := "0123456789abcdef0123456789abcdef01234567"

	// 1. First claim succeeds
	claimed, rec, err := ledger.ClaimDecision(ctx, prKey, headSHA, ghclient.EventApprove, "job-1")
	if err != nil {
		t.Fatalf("unexpected claim error: %v", err)
	}
	if !claimed || rec == nil {
		t.Fatalf("expected claimed=true, got claimed=%v rec=%v", claimed, rec)
	}
	if rec.Status != DecisionStatusClaimed || rec.Event != "APPROVE" || rec.JobID != "job-1" {
		t.Errorf("unexpected record fields: %+v", rec)
	}

	// 2. Second claim for same head returns duplicate without error
	claimed2, rec2, err2 := ledger.ClaimDecision(ctx, prKey, headSHA, ghclient.EventRequestChanges, "job-2")
	if err2 != nil {
		t.Fatalf("unexpected second claim error: %v", err2)
	}
	if claimed2 {
		t.Fatalf("expected claimed2=false for duplicate head, got true")
	}
	if rec2 == nil || rec2.JobID != "job-1" {
		t.Fatalf("expected rec2 to reference initial job-1, got %+v", rec2)
	}

	// 3. Mark published
	if err := ledger.RecordPublished(ctx, prKey, headSHA, 9999); err != nil {
		t.Fatalf("failed recording published: %v", err)
	}

	// 4. Third claim after published also returns duplicate
	claimed3, rec3, err3 := ledger.ClaimDecision(ctx, prKey, headSHA, ghclient.EventApprove, "job-3")
	if err3 != nil {
		t.Fatalf("unexpected third claim error: %v", err3)
	}
	if claimed3 || rec3.Status != DecisionStatusPublished || rec3.ReviewID != 9999 {
		t.Fatalf("expected claimed3=false with published status, got claimed=%v rec=%+v", claimed3, rec3)
	}
}

func TestPublishDecision_SameHeadZeroAdditionalHTTPCalls(t *testing.T) {
	store, ledger, _ := setupTestLedger(t)
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 100, 1)
	headSHA := "0123456789abcdef0123456789abcdef01234567"

	job1 := &Job{
		ID:       "job-1",
		PRKey:    prKey,
		Owner:    "owner",
		Repo:     "repo",
		PRNumber: 1,
		HeadSHA:  headSHA,
	}

	mockGH := &mockDecisionClient{}

	// First publish succeeds and calls GitHub once
	res1, err := PublishDecision(ctx, ledger, mockGH, job1, ghclient.EventApprove, "body 1", nil, nil)
	if err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}
	if res1 == nil || res1.Duplicate || res1.Review == nil {
		t.Fatalf("expected non-duplicate result with review, got %+v", res1)
	}
	if mockGH.CallCount() != 1 {
		t.Fatalf("expected exactly 1 GitHub call, got %d", mockGH.CallCount())
	}

	// Second publish with same head must return duplicate with ZERO additional HTTP calls
	job2 := &Job{
		ID:       "job-2",
		PRKey:    prKey,
		Owner:    "owner",
		Repo:     "repo",
		PRNumber: 1,
		HeadSHA:  headSHA,
	}

	res2, err := PublishDecision(ctx, ledger, mockGH, job2, ghclient.EventRequestChanges, "body 2", nil, nil)
	if !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("expected ErrDuplicateDecision, got: %v", err)
	}
	if res2 == nil || !res2.Duplicate {
		t.Fatalf("expected duplicate result, got: %+v", res2)
	}
	if mockGH.CallCount() != 1 {
		t.Fatalf("expected still exactly 1 GitHub call (0 additional), got %d", mockGH.CallCount())
	}
}

func TestPublishDecision_ConcurrentDoubleClaimYieldsExactlyOneCreate(t *testing.T) {
	store, ledger, _ := setupTestLedger(t)
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 100, 1)
	headSHA := "0123456789abcdef0123456789abcdef01234567"

	mockGH := &mockDecisionClient{
		postReviewFn: func(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []ghclient.InlineSuggestion) (*github.PullRequestReview, error) {
			// Small sleep to encourage race window
			time.Sleep(20 * time.Millisecond)
			return &github.PullRequestReview{ID: github.Ptr(int64(8888))}, nil
		},
	}

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)

	successCount := atomic.Int32{}
	duplicateCount := atomic.Int32{}

	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			job := &Job{
				ID:       "job-concurrent",
				PRKey:    prKey,
				Owner:    "owner",
				Repo:     "repo",
				PRNumber: 1,
				HeadSHA:  headSHA,
			}
			res, err := PublishDecision(ctx, ledger, mockGH, job, ghclient.EventApprove, "concurrent body", nil, nil)
			if err == nil && res != nil && !res.Duplicate {
				successCount.Add(1)
			} else if errors.Is(err, ErrDuplicateDecision) && res != nil && res.Duplicate {
				duplicateCount.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("expected exactly 1 successful decision publication, got %d", successCount.Load())
	}
	if duplicateCount.Load() != workers-1 {
		t.Fatalf("expected %d duplicates, got %d", workers-1, duplicateCount.Load())
	}
	if mockGH.CallCount() != 1 {
		t.Fatalf("expected exactly 1 remote create call, got %d", mockGH.CallCount())
	}
}

func TestPublishDecision_UncertainWriteYieldsNeedsAttentionOnce(t *testing.T) {
	store, ledger, _ := setupTestLedger(t)
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 100, 1)
	headSHA := "0123456789abcdef0123456789abcdef01234567"

	job := &Job{
		ID:       "job-timeout",
		PRKey:    prKey,
		Owner:    "owner",
		Repo:     "repo",
		PRNumber: 1,
		HeadSHA:  headSHA,
	}

	mockGH := &mockDecisionClient{
		postReviewFn: func(ctx context.Context, owner, repo string, number int, commitSHA string, event string, body string, suggestions []ghclient.InlineSuggestion) (*github.PullRequestReview, error) {
			// Injected write timeout or server error -> ClassUncertainWrite
			return nil, &ghclient.DecisionWriteError{
				Err:            errors.New("POST timeout to GitHub"),
				Classification: retry.ClassUncertainWrite,
			}
		},
	}

	var needsAttentionCalled atomic.Int32
	var capturedReason string

	markNeedsAttention := func(ctx context.Context, j *Job, reason string) {
		needsAttentionCalled.Add(1)
		capturedReason = reason
		j.Status = "needs_attention"
	}

	_, err := PublishDecision(ctx, ledger, mockGH, job, ghclient.EventApprove, "body", nil, markNeedsAttention)
	if err == nil {
		t.Fatalf("expected error from PublishDecision, got nil")
	}

	if needsAttentionCalled.Load() != 1 {
		t.Fatalf("expected markNeedsAttention called exactly once, got %d", needsAttentionCalled.Load())
	}
	if mockGH.CallCount() != 1 {
		t.Fatalf("expected exactly 1 remote attempt, got %d", mockGH.CallCount())
	}
	if job.Status != "needs_attention" {
		t.Errorf("expected job.Status to be needs_attention, got %q", job.Status)
	}

	// Verify ledger marked state uncertain
	dec, err := ledger.GetDecision(ctx, prKey, headSHA)
	if err != nil || dec == nil {
		t.Fatalf("failed retrieving decision from ledger: %v", err)
	}
	if dec.Status != DecisionStatusUncertain {
		t.Errorf("expected decision status uncertain, got %q", dec.Status)
	}
	_ = capturedReason
}

func TestDecisionLedger_ReopenStoreMaintainsDuplicate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reopen_ledger.db")

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 200, 2)
	headSHA := "0123456789abcdef0123456789abcdef01234567"

	// 1. Open store, record published decision, then close
	{
		store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatalf("failed opening store: %v", err)
		}

		ledger := NewDecisionLedger(store.DB())
		claimed, _, err := ledger.ClaimDecision(ctx, prKey, headSHA, ghclient.EventApprove, "job-reopen")
		if err != nil || !claimed {
			t.Fatalf("claim failed: claimed=%v, err=%v", claimed, err)
		}
		if err := ledger.RecordPublished(ctx, prKey, headSHA, 5555); err != nil {
			t.Fatalf("record published failed: %v", err)
		}

		if err := store.Close(); err != nil {
			t.Fatalf("failed closing store: %v", err)
		}
	}

	// 2. Re-open store from same file
	{
		store2, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatalf("failed reopening store: %v", err)
		}
		defer store2.Close()

		ledger2 := NewDecisionLedger(store2.DB())

		// GetDecision proves persistence across restart
		dec, err := ledger2.GetDecision(ctx, prKey, headSHA)
		if err != nil {
			t.Fatalf("unexpected error getting decision: %v", err)
		}
		if dec == nil || dec.Status != DecisionStatusPublished || dec.ReviewID != 5555 {
			t.Fatalf("expected persisted published decision, got: %+v", dec)
		}

		// New claim must be rejected as duplicate
		claimed, existing, err := ledger2.ClaimDecision(ctx, prKey, headSHA, ghclient.EventApprove, "job-after-restart")
		if err != nil {
			t.Fatalf("unexpected claim error: %v", err)
		}
		if claimed {
			t.Fatalf("expected duplicate claim after restart to return claimed=false")
		}
		if existing == nil || existing.ReviewID != 5555 {
			t.Fatalf("expected existing record with ReviewID 5555, got: %+v", existing)
		}
	}
}

func TestDecisionLedger_Pruning(t *testing.T) {
	store, ledger, _ := setupTestLedger(t)
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 300, 3)
	shaOld := "0123456789abcdef0123456789abcdef01234567"
	shaNew := "1111111111111111111111111111111111111111"

	// Record an old published decision
	_, _, _ = ledger.ClaimDecision(ctx, prKey, shaOld, ghclient.EventApprove, "job-old")
	_ = ledger.RecordPublished(ctx, prKey, shaOld, 101)

	// Backdate the published_at time in bbolt
	err := store.DB().Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDecisions)
		key := decisionLedgerKey(prKey, shaOld)
		var rec DecisionRecord
		_ = json.Unmarshal(b.Get(key), &rec)
		tOld := time.Now().UTC().Add(-48 * time.Hour)
		rec.PublishedAt = &tOld
		raw, _ := json.Marshal(&rec)
		return b.Put(key, raw)
	})
	if err != nil {
		t.Fatalf("failed backdating record: %v", err)
	}

	// Record a fresh published decision
	_, _, _ = ledger.ClaimDecision(ctx, prKey, shaNew, ghclient.EventApprove, "job-new")
	_ = ledger.RecordPublished(ctx, prKey, shaNew, 102)

	// Prune older than 24h
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := ledger.PruneTerminalDecisions(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("prune failed: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 record pruned, got %d", deleted)
	}

	// shaOld should be gone
	decOld, _ := ledger.GetDecision(ctx, prKey, shaOld)
	if decOld != nil {
		t.Errorf("expected shaOld to be pruned, but found: %+v", decOld)
	}

	// shaNew should remain
	decNew, _ := ledger.GetDecision(ctx, prKey, shaNew)
	if decNew == nil || decNew.ReviewID != 102 {
		t.Errorf("expected shaNew to remain, got: %+v", decNew)
	}
}
