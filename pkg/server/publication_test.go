package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
