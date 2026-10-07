package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"


	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
)

func seedTestJob(t *testing.T, store JobStore, prKey PRKey, baseSHA, headSHA string) *Job {
	delivery := Delivery{
		Host:        prKey.Host,
		RepoID:      prKey.RepoID,
		DeliveryID:  fmt.Sprintf("deliv-%d", time.Now().UnixNano()),
		EventKind:   "pull_request",
		PayloadHash: "hash",
		ReceivedAt:  time.Now().UTC(),
	}
	res, err := store.Admit(context.Background(), delivery, []Job{
		{
			Kind:     "review",
			Trigger:  "automatic",
			PRKey:    prKey,
			Owner:    "owner",
			Repo:     "repo",
			PRNumber: prKey.Number,
			BaseSHA:  baseSHA,
			HeadSHA:  headSHA,
		},
	})
	if err != nil || res.Status != AdmitAccepted {
		t.Fatalf("seedTestJob Admit failed: %v, status: %v", err, res.Status)
	}
	queued, err := store.ListQueuedJobs(context.Background())
	if err != nil || len(queued) == 0 {
		t.Fatalf("seedTestJob ListQueuedJobs failed: %v", err)
	}
	return queued[len(queued)-1]
}

func TestPublicationRecovery(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	headSHA := "2222222222222222222222222222222222222222"
	prKey, _ := MakePRKey("github.com", 1234, 1)

	t.Run("Crash Window 1: Reconcile before send posts comment and transitions to completed", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatalf("OpenJobStore error: %v", err)
		}
		defer store.Close()

		ctx := context.Background()

		job := seedTestJob(t, store, prKey, baseSHA, headSHA)

		marker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
		body := "## Review Report\nLooks great!\n\n" + marker
		h := sha256.Sum256([]byte(body))
		bodyDigest := hex.EncodeToString(h[:])

		intent := &OutputIntent{
			Marker:     marker,
			JobID:      job.ID,
			Action:     "review_output",
			PRKey:      prKey,
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   1,
			ExactHead:  headSHA,
			Body:       body,
			BodyDigest: bodyDigest,
			Status:     "pending",
		}
		_ = store.SaveOutputIntent(ctx, intent)

		var createCommentCalls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case r.URL.Path == "/repos/owner/repo/pulls/1":
				json.NewEncoder(w).Encode(map[string]any{
					"number": 1,
					"base":   map[string]any{"sha": baseSHA},
					"head":   map[string]any{"sha": headSHA},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodGet:
				json.NewEncoder(w).Encode([]any{})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodPost:
				atomic.AddInt32(&createCommentCalls, 1)
				json.NewEncoder(w).Encode(map[string]any{"id": 501, "body": body})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		ghClient, _ := ghclient.NewTestClient(ts.URL)
		pub := NewPublication(store, ghClient)

		res, err := pub.ReconcileOutput(ctx, intent)
		if err != nil {
			t.Fatalf("ReconcileOutput error: %v", err)
		}
		if res.Status != "completed" || res.CommentID != 501 {
			t.Errorf("got res %+v, want status completed and CommentID 501", res)
		}

		if atomic.LoadInt32(&createCommentCalls) != 1 {
			t.Errorf("expected exactly 1 CreateComment call, got %d", createCommentCalls)
		}

		updatedJob, _ := store.GetJob(ctx, job.ID)
		if updatedJob.Status != "completed" {
			t.Errorf("job status = %q, want completed", updatedJob.Status)
		}

		updatedIntent, _ := store.GetOutputIntent(ctx, marker)
		if updatedIntent.Status != "completed" || updatedIntent.CommentID != 501 {
			t.Errorf("intent status = %q, commentID = %d", updatedIntent.Status, updatedIntent.CommentID)
		}

		prState, _ := store.GetPRState(ctx, prKey)
		if prState.LastReviewedHead != headSHA {
			t.Errorf("LastReviewedHead = %q, want %q", prState.LastReviewedHead, headSHA)
		}
	})

	t.Run("Crash Window 2: Reconcile after remote commit recovers comment ID without duplicate post", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatalf("OpenJobStore error: %v", err)
		}
		defer store.Close()

		ctx := context.Background()

		job := seedTestJob(t, store, prKey, baseSHA, headSHA)

		marker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
		body := "## Review Report\nRemote commit succeeded before crash!\n\n" + marker
		h := sha256.Sum256([]byte(body))
		bodyDigest := hex.EncodeToString(h[:])

		intent := &OutputIntent{
			Marker:     marker,
			JobID:      job.ID,
			Action:     "review_output",
			PRKey:      prKey,
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   1,
			ExactHead:  headSHA,
			Body:       body,
			BodyDigest: bodyDigest,
			Status:     "pending",
			CommentID:  0, // Local store crashed before CommentID was recorded!
		}
		_ = store.SaveOutputIntent(ctx, intent)

		var postCommentCalls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case r.URL.Path == "/repos/owner/repo/pulls/1":
				json.NewEncoder(w).Encode(map[string]any{
					"number": 1,
					"base":   map[string]any{"sha": baseSHA},
					"head":   map[string]any{"sha": headSHA},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodGet:
				// Remote comment ALREADY exists on GitHub!
				comments := []map[string]any{
					{
						"id":   602,
						"body": body,
						"user": map[string]any{"id": 100, "login": "bot-user"},
					},
				}
				json.NewEncoder(w).Encode(comments)
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodPost:
				atomic.AddInt32(&postCommentCalls, 1)
				json.NewEncoder(w).Encode(map[string]any{"id": 999})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		ghClient, _ := ghclient.NewTestClient(ts.URL)
		pub := NewPublication(store, ghClient)

		res, err := pub.ReconcileOutput(ctx, intent)
		if err != nil {
			t.Fatalf("ReconcileOutput error: %v", err)
		}

		if res.Status != "completed" || res.CommentID != 602 || !res.Reused {
			t.Errorf("got res %+v, want status completed, CommentID 602, Reused true", res)
		}

		if atomic.LoadInt32(&postCommentCalls) != 0 {
			t.Errorf("PostComment must NOT be called when comment already exists remotely; called %d times", postCommentCalls)
		}

		updatedIntent, _ := store.GetOutputIntent(ctx, marker)
		if updatedIntent.CommentID != 602 {
			t.Errorf("intent CommentID = %d, want 602", updatedIntent.CommentID)
		}
	})

	t.Run("Status Comment Recycling: patches one comment across transitions (D-05)", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "jobs.db")
		store, _ := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		defer store.Close()

		ctx := context.Background()

		job := seedTestJob(t, store, prKey, baseSHA, headSHA)

		var createCalls int32
		var editCalls int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodGet:
				json.NewEncoder(w).Encode([]any{})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodPost:
				atomic.AddInt32(&createCalls, 1)
				json.NewEncoder(w).Encode(map[string]any{"id": 703})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/comments/703") && r.Method == http.MethodPatch:
				atomic.AddInt32(&editCalls, 1)
				json.NewEncoder(w).Encode(map[string]any{"id": 703})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		ghClient, _ := ghclient.NewTestClient(ts.URL)
		pub := NewPublication(store, ghClient)

		// 1. Initial queued status -> Creates comment 703
		id1, err := pub.PublishStatus(ctx, job, "⏳ Review queued; waiting for capacity")
		if err != nil || id1 != 703 {
			t.Fatalf("PublishStatus queued error: %v, id: %d", err, id1)
		}

		// 2. Running transition -> Edits comment 703
		id2, err := pub.PublishStatus(ctx, job, "🔄 Review running...")
		if err != nil || id2 != 703 {
			t.Fatalf("PublishStatus running error: %v, id: %d", err, id2)
		}

		// 3. Completed transition -> Edits comment 703
		id3, err := pub.PublishStatus(ctx, job, "✅ Review completed.")
		if err != nil || id3 != 703 {
			t.Fatalf("PublishStatus completed error: %v, id: %d", err, id3)
		}

		if atomic.LoadInt32(&createCalls) != 1 {
			t.Errorf("expected exactly 1 CreateComment call across transitions, got %d", createCalls)
		}
		if atomic.LoadInt32(&editCalls) != 2 {
			t.Errorf("expected exactly 2 EditComment calls, got %d", editCalls)
		}
	})

	t.Run("Head moved in-flight marks superseded without relabelling (D-02)", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "jobs.db")
		store, _ := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		defer store.Close()

		ctx := context.Background()

		job := seedTestJob(t, store, prKey, baseSHA, headSHA)

		marker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
		body := "## Review Report\nAnalysis of old head\n\n" + marker
		newHeadSHA := "3333333333333333333333333333333333333333"

		intent := &OutputIntent{
			Marker:    marker,
			JobID:     job.ID,
			Action:    "review_output",
			PRKey:     prKey,
			Owner:     job.Owner,
			Repo:      job.Repo,
			PRNumber:  1,
			ExactHead: headSHA,
			Body:      body,
			Status:    "pending",
		}
		_ = store.SaveOutputIntent(ctx, intent)

		getPRCall := 0
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case r.URL.Path == "/repos/owner/repo/pulls/1":
				getPRCall++
				if getPRCall == 1 {
					// Pre-write check: still headSHA
					json.NewEncoder(w).Encode(map[string]any{
						"number": 1,
						"base":   map[string]any{"sha": baseSHA},
						"head":   map[string]any{"sha": headSHA},
					})
				} else {
					// Post-write check: Head moved in-flight to newHeadSHA!
					json.NewEncoder(w).Encode(map[string]any{
						"number": 1,
						"base":   map[string]any{"sha": baseSHA},
						"head":   map[string]any{"sha": newHeadSHA},
					})
				}
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodGet:
				json.NewEncoder(w).Encode([]any{})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/1/comments") && r.Method == http.MethodPost:
				json.NewEncoder(w).Encode(map[string]any{"id": 804})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/comments/804") && r.Method == http.MethodPatch:
				json.NewEncoder(w).Encode(map[string]any{"id": 804})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		ghClient, _ := ghclient.NewTestClient(ts.URL)
		pub := NewPublication(store, ghClient)

		res, err := pub.ReconcileOutput(ctx, intent)
		if err != nil {
			t.Fatalf("ReconcileOutput error: %v", err)
		}
		if res.Status != "superseded" {
			t.Errorf("got status %q, want superseded", res.Status)
		}

		updatedJob, _ := store.GetJob(ctx, job.ID)
		if updatedJob.Status != "superseded" {
			t.Errorf("job status = %q, want superseded", updatedJob.Status)
		}

		// Verify successor review was scheduled for the new head!
		queued, _ := store.ListQueuedJobs(ctx)
		foundSuccessor := false
		for _, q := range queued {
			if q.HeadSHA == newHeadSHA {
				foundSuccessor = true
				break
			}
		}
		if !foundSuccessor {
			t.Errorf("expected successor review to be scheduled for new head %s", newHeadSHA)
		}
	})

	t.Run("Stored output reconciles without expensive regeneration", func(t *testing.T) {
		tempDir := t.TempDir()
		dbPath := filepath.Join(tempDir, "jobs.db")
		store, _ := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		defer store.Close()

		ctx := context.Background()

		job := seedTestJob(t, store, prKey, baseSHA, headSHA)
		job.StatusCommentID = 901
		_ = store.UpdateJob(ctx, job)

		marker := fmt.Sprintf("<!-- pr-review-output:%s -->", job.ID)
		body := "## Saved Review Output\nDurable generated markdown\n\n" + marker
		h := sha256.Sum256([]byte(body))
		bodyDigest := hex.EncodeToString(h[:])

		intent := &OutputIntent{
			Marker:     marker,
			JobID:      job.ID,
			Action:     "review_output",
			PRKey:      prKey,
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   1,
			ExactHead:  headSHA,
			Body:       body,
			BodyDigest: bodyDigest,
			Status:     "pending",
			CommentID:  905, // already posted remotely
		}
		_ = store.SaveOutputIntent(ctx, intent)

		var engineCalled int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/comments/905") && r.Method == http.MethodGet:
				json.NewEncoder(w).Encode(map[string]any{
					"id":   905,
					"body": body,
					"user": map[string]any{"id": 100, "login": "bot-user"},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/comments/901") && r.Method == http.MethodPatch:
				json.NewEncoder(w).Encode(map[string]any{"id": 901})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		ghClient, _ := ghclient.NewTestClient(ts.URL)
		mockEngine := reviewer.NewEngineWithClients(&config.Config{}, ghClient, nil, nil)

		srv := &Server{
			gh:     ghClient,
			engine: mockEngine,
			store:  store,
		}
		executor := NewServerJobExecutor(srv, store)

		// Calling executeReviewJob should reconcile the stored intent directly!
		err := executor.executeReviewJob(ctx, job)
		if err != nil {
			t.Fatalf("executeReviewJob error: %v", err)
		}

		if atomic.LoadInt32(&engineCalled) != 0 {
			t.Errorf("review engine must not be called when output intent is stored")
		}

		updatedJob, _ := store.GetJob(ctx, job.ID)
		if updatedJob.Status != "completed" {
			t.Errorf("job status = %q, want completed", updatedJob.Status)
		}
	})
}

func TestJobRecovery(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "jobs.db")
	store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 20})
	if err != nil {
		t.Fatalf("OpenJobStore error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	prKey1, _ := MakePRKey("github.com", 100, 1)
	prKey2, _ := MakePRKey("github.com", 200, 2)
	baseSHA := "1111111111111111111111111111111111111111"
	headSHA := "2222222222222222222222222222222222222222"

	// Admit jobs for PR 1
	deliv1 := Delivery{
		Host:        prKey1.Host,
		RepoID:      prKey1.RepoID,
		DeliveryID:  "deliv-1",
		EventKind:   "pull_request",
		PayloadHash: "hash1",
		ReceivedAt:  time.Now().UTC(),
	}
	_, err = store.Admit(ctx, deliv1, []Job{
		{Kind: "review", Trigger: "automatic", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
		{Kind: "review", Trigger: "automatic", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
		{Kind: "labels", Trigger: "automatic", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
		{Kind: "improve", Trigger: "explicit", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
		{Kind: "summary", Trigger: "explicit", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
		{Kind: "assistant", Trigger: "explicit", PRKey: prKey1, Owner: "org", Repo: "repo1", PRNumber: 1, BaseSHA: baseSHA, HeadSHA: headSHA},
	})
	if err != nil {
		t.Fatalf("Admit deliv1 error: %v", err)
	}

	// Admit queued job for PR 2
	deliv2 := Delivery{
		Host:        prKey2.Host,
		RepoID:      prKey2.RepoID,
		DeliveryID:  "deliv-2",
		EventKind:   "pull_request",
		PayloadHash: "hash2",
		ReceivedAt:  time.Now().UTC(),
	}
	_, err = store.Admit(ctx, deliv2, []Job{
		{Kind: "review", Trigger: "automatic", PRKey: prKey2, Owner: "org", Repo: "repo2", PRNumber: 2, BaseSHA: baseSHA, HeadSHA: headSHA},
	})
	if err != nil {
		t.Fatalf("Admit deliv2 error: %v", err)
	}

	jobs, _ := store.ListQueuedJobs(ctx)
	if len(jobs) < 7 {
		t.Fatalf("expected at least 7 queued jobs, got %d", len(jobs))
	}

	// Mark jobs 0-5 as running (simulating crash while running)
	j0 := jobs[0] // review with saved output
	j0.Status = "running"
	_ = store.UpdateJob(ctx, j0)
	// Save output intent for j0
	marker0 := fmt.Sprintf("<!-- pr-review-output:%s -->", j0.ID)
	_ = store.SaveOutputIntent(ctx, &OutputIntent{
		Marker: marker0, JobID: j0.ID, Action: "review_output", PRKey: prKey1,
		ExactHead: headSHA, Body: "Saved output report", Status: "pending",
	})

	j1 := jobs[1] // review without saved output
	j1.Status = "running"
	_ = store.UpdateJob(ctx, j1)

	j2 := jobs[2] // labels (idempotent)
	j2.Status = "running"
	_ = store.UpdateJob(ctx, j2)

	j3 := jobs[3] // improve (static)
	j3.Status = "running"
	_ = store.UpdateJob(ctx, j3)

	j4 := jobs[4] // summary (non-idempotent)
	j4.Status = "running"
	_ = store.UpdateJob(ctx, j4)

	j5 := jobs[5] // assistant (non-idempotent)
	j5.Status = "running"
	_ = store.UpdateJob(ctx, j5)

	// Set active job for PR 1
	st1, _ := store.GetPRState(ctx, prKey1)
	st1.ActiveJobID = j0.ID
	_ = store.UpdatePRState(ctx, st1)

	// Perform RecoverJobs
	recovered, err := store.RecoverJobs(ctx)
	if err != nil {
		t.Fatalf("RecoverJobs error: %v", err)
	}
	if len(recovered) != 6 {
		t.Fatalf("expected 6 recovered jobs, got %d", len(recovered))
	}

	// Verify recovery phases and statuses:
	rj0, _ := store.GetJob(ctx, j0.ID)
	if rj0.Status != "queued" || rj0.RecoveryPhase != "reconcile_output" {
		t.Errorf("job 0 with saved output: status = %q, phase = %q, want queued / reconcile_output", rj0.Status, rj0.RecoveryPhase)
	}

	rj1, _ := store.GetJob(ctx, j1.ID)
	if rj1.Status != "queued" || rj1.RecoveryPhase != "requeued_after_interrupt" {
		t.Errorf("job 1 without output: status = %q, phase = %q, want queued / requeued_after_interrupt", rj1.Status, rj1.RecoveryPhase)
	}

	rj2, _ := store.GetJob(ctx, j2.ID)
	if rj2.Status != "queued" || rj2.RecoveryPhase != "requeued_idempotent" {
		t.Errorf("job 2 labels: status = %q, phase = %q, want queued / requeued_idempotent", rj2.Status, rj2.RecoveryPhase)
	}

	rj3, _ := store.GetJob(ctx, j3.ID)
	if rj3.Status != "queued" || rj3.RecoveryPhase != "requeued_static" {
		t.Errorf("job 3 improve: status = %q, phase = %q, want queued / requeued_static", rj3.Status, rj3.RecoveryPhase)
	}

	rj4, _ := store.GetJob(ctx, j4.ID)
	if rj4.Status != "needs_attention" || rj4.RecoveryPhase != "interrupted_unproven" {
		t.Errorf("job 4 summary: status = %q, phase = %q, want needs_attention / interrupted_unproven", rj4.Status, rj4.RecoveryPhase)
	}

	rj5, _ := store.GetJob(ctx, j5.ID)
	if rj5.Status != "needs_attention" || rj5.RecoveryPhase != "interrupted_unproven" {
		t.Errorf("job 5 assistant: status = %q, phase = %q, want needs_attention / interrupted_unproven", rj5.Status, rj5.RecoveryPhase)
	}

	// Verify PR states: PR 1 should be blocked because of needs_attention jobs, active slot cleared
	st1After, _ := store.GetPRState(ctx, prKey1)
	if st1After.ActiveJobID != "" {
		t.Errorf("expected PR 1 ActiveJobID to be cleared, got %q", st1After.ActiveJobID)
	}
	if !st1After.HasBlockedAction {
		t.Errorf("expected PR 1 to have HasBlockedAction = true")
	}

	// Verify ClaimNextJob: PR 1 jobs are blocked, but PR 2 job can be claimed!
	claimedJob, err := store.ClaimNextJob(ctx, map[string]bool{})
	if err != nil {
		t.Fatalf("ClaimNextJob error: %v", err)
	}
	if claimedJob == nil {
		t.Fatalf("expected PR 2 job to be claimed, got nil")
	}
	if claimedJob.PRKey != prKey2 {
		t.Errorf("claimed job PRKey = %v, want PR 2 (%v)", claimedJob.PRKey, prKey2)
	}
}

func TestQueueOperatorResolution(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "jobs.db")
	store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
	if err != nil {
		t.Fatalf("OpenJobStore error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 300, 3)
	job := seedTestJob(t, store, prKey, "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")

	// 1. InspectJobs
	allJobs, err := store.InspectJobs(ctx)
	if err != nil {
		t.Fatalf("InspectJobs error: %v", err)
	}
	if len(allJobs) == 0 {
		t.Fatalf("InspectJobs returned empty list")
	}

	// 2. Unknown job resolution returns ErrJobNotFound
	err = store.ResolveJob(ctx, "nonexistent-job-id", "confirmed", false)
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("expected ErrJobNotFound, got: %v", err)
	}

	// 3. Invalid resolution returns error
	err = store.ResolveJob(ctx, job.ID, "bogus-disposition", false)
	if err == nil {
		t.Errorf("expected error for invalid resolution disposition")
	}

	// 4. Rerun without acknowledge-duplicate-risk returns error
	job.Status = "needs_attention"
	_ = store.UpdateJob(ctx, job)
	err = store.ResolveJob(ctx, job.ID, "rerun", false)
	if err == nil || !strings.Contains(err.Error(), "--acknowledge-duplicate-risk") {
		t.Errorf("expected acknowledge duplicate risk error, got: %v", err)
	}

	// 5. Rerun with acknowledge-duplicate-risk succeeds and requeues job
	err = store.ResolveJob(ctx, job.ID, "rerun", true)
	if err != nil {
		t.Fatalf("ResolveJob rerun error: %v", err)
	}
	rerunJob, _ := store.GetJob(ctx, job.ID)
	if rerunJob.Status != "queued" || rerunJob.RecoveryPhase != "operator_rerun" {
		t.Errorf("rerun job status = %q, phase = %q", rerunJob.Status, rerunJob.RecoveryPhase)
	}

	// 6. Confirmed disposition marks job completed
	err = store.ResolveJob(ctx, job.ID, "confirmed", false)
	if err != nil {
		t.Fatalf("ResolveJob confirmed error: %v", err)
	}
	confirmedJob, _ := store.GetJob(ctx, job.ID)
	if confirmedJob.Status != "completed" || confirmedJob.RecoveryPhase != "operator_confirmed" {
		t.Errorf("confirmed job status = %q, phase = %q", confirmedJob.Status, confirmedJob.RecoveryPhase)
	}

	// 7. Cancel disposition marks job cancelled
	job.Status = "needs_attention"
	_ = store.UpdateJob(ctx, job)
	err = store.ResolveJob(ctx, job.ID, "cancel", false)
	if err != nil {
		t.Fatalf("ResolveJob cancel error: %v", err)
	}
	cancelledJob, _ := store.GetJob(ctx, job.ID)
	if cancelledJob.Status != "cancelled" || cancelledJob.RecoveryPhase != "operator_cancelled" {
		t.Errorf("cancelled job status = %q, phase = %q", cancelledJob.Status, cancelledJob.RecoveryPhase)
	}

	// 8. Second-writer access: opening store while already open returns ErrDatabaseLocked
	_, errSecond := OpenJobStore(dbPath, StoreOptions{OpenTimeout: 50 * time.Millisecond})
	if !errors.Is(errSecond, ErrDatabaseLocked) {
		t.Errorf("expected ErrDatabaseLocked for second writer, got: %v", errSecond)
	}
}

func TestGracefulShutdown(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		WebhookStateDir:        tempDir,
		WebhookSecret:          "test-secret",
		WebhookWorkers:         1,
		WebhookShutdownTimeout: 5 * time.Second,
		Port:                   8080,
		GitHubToken:            "test-token",
		LLMAPIKey:              "test-key",
		LLMModel:               "gpt-4o",
		LLMBaseURL:             "https://api.openai.com/v1",
	}

	srv := NewServer(cfg)
	ctx := context.Background()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Server.Start error: %v", err)
	}

	// 1. Health check returns non-sensitive counts
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health check returned code %d", rec.Code)
	}
	var healthResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &healthResp); err != nil {
		t.Fatalf("decode health response error: %v", err)
	}
	if healthResp["status"] != "ok" || healthResp["runtime"] != "ready" {
		t.Errorf("health status = %v, runtime = %v", healthResp["status"], healthResp["runtime"])
	}
	if _, ok := healthResp["counts"]; !ok {
		t.Errorf("health response missing non-sensitive counts: %v", healthResp)
	}

	// 2. Initiate Shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Server.Shutdown error: %v", err)
	}

	// 3. Webhook admission after shutdown receives 503
	webhookRec := httptest.NewRecorder()
	webhookReq := httptest.NewRequest(http.MethodPost, "/api/v1/github_webhooks", strings.NewReader(`{}`))
	srv.Routes().ServeHTTP(webhookRec, webhookReq)

	if webhookRec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable during/after shutdown, got %d", webhookRec.Code)
	}

	// 4. Setup mode has no queue lifecycle
	setupCfg := &config.Config{
		GitHubAppSetupToken: "setup-token-123",
		PublicURL:           "https://example.com",
	}
	setupSrv := NewServer(setupCfg)
	if err := setupSrv.Start(context.Background()); err != nil {
		t.Fatalf("setup mode Start error: %v", err)
	}
	if err := setupSrv.Shutdown(context.Background()); err != nil {
		t.Fatalf("setup mode Shutdown error: %v", err)
	}
}
