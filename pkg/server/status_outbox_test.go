package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
)

func TestQueuedStatusOutbox(t *testing.T) {
	t.Run("DispatchesPendingIntentsWhenWorkersIdleOrBlocked", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		key, _ := MakePRKey("github.com", 1234, 1)
		job := seedTestJob(t, store, key, "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")

		var posted atomic.Int32
		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case r.Method == "GET" && r.URL.Path == "/repos/owner/repo/issues/1/comments":
				_ = json.NewEncoder(w).Encode([]any{})
			case r.Method == "POST" && r.URL.Path == "/repos/owner/repo/issues/1/comments":
				posted.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 501, "body": "queued"})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		gh, err := ghclient.NewTestClient(ghServer.URL)
		if err != nil {
			t.Fatal(err)
		}

		outbox := NewStatusOutbox(store, gh, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		outbox.Start(ctx)
		defer outbox.Stop()

		outbox.Wake()

		for i := 0; i < 50; i++ {
			if posted.Load() >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		if posted.Load() != 1 {
			t.Fatalf("expected 1 posted queued status, got %d", posted.Load())
		}

		// Verify job has StatusCommentID set
		var updatedJob *Job
		for i := 0; i < 100; i++ {
			j, err := store.GetJob(ctx, job.ID)
			if err == nil && j != nil && j.StatusCommentID == 501 {
				updatedJob = j
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if updatedJob == nil {
			j, _ := store.GetJob(ctx, job.ID)
			val := int64(0)
			if j != nil {
				val = j.StatusCommentID
			}
			t.Fatalf("expected StatusCommentID 501, got %d", val)
		}

		// Verify intent is marked completed
		marker := fmt.Sprintf("<!-- pr-review-status:%s -->", job.ID)
		intent, err := store.GetOutputIntent(ctx, marker)
		if err != nil {
			t.Fatal(err)
		}
		if intent.Status != "completed" {
			t.Fatalf("expected intent status completed, got %s", intent.Status)
		}
	})

	t.Run("DelayedQueuedWriteDoesNotOverwriteRunningOrTerminalStatus", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		key, _ := MakePRKey("github.com", 1234, 1)
		job := seedTestJob(t, store, key, "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")

		// Move job to running
		job.Status = "running"
		if err := store.UpdateJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}

		var posted atomic.Int32
		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/user":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "login": "bot-user"})
			case r.Method == "POST":
				posted.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 501})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		gh, _ := ghclient.NewTestClient(ghServer.URL)
		outbox := NewStatusOutbox(store, gh, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		outbox.Start(ctx)
		defer outbox.Stop()

		outbox.Wake()
		time.Sleep(100 * time.Millisecond)

		if posted.Load() != 0 {
			t.Fatalf("delayed queued write must not post to GitHub for running job; got %d posts", posted.Load())
		}
	})
}
