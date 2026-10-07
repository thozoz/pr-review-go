package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
)

func TestWebhookDurableTracer(t *testing.T) {
	const (
		testSecret   = "tracer-webhook-secret"
		validBaseSHA = "1111111111111111111111111111111111111111"
		validHeadSHA = "2222222222222222222222222222222222222222"
		repoID       = int64(12345)
		prNum        = 10
	)

	var (
		llmCalls     int32
		compareCalls int32
		createdComments []string
		editedComments  []string
		commentMu       sync.Mutex
		commentCounter  int64 = 1000
	)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		resp := llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Content: `{"score": 92, "summary": "Automatic review passed", "findings": []}`,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, fmt.Sprintf("/pulls/%d", prNum)):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": prNum,
				"title":  "Tracer Pull Request",
				"body":   "Initial body",
				"head":   map[string]any{"sha": validHeadSHA, "ref": "feat-tracer"},
				"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
				"user":   map[string]any{"login": "tracer-dev"},
			})
		case strings.Contains(r.URL.Path, "/compare/"):
			atomic.AddInt32(&compareCalls, 1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("diff --git a/main.go b/main.go\n+func Tracer() {}\n"))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, fmt.Sprintf("/issues/%d/comments", prNum)):
			commentMu.Lock()
			newID := atomic.AddInt64(&commentCounter, 1)
			var bodyMap map[string]string
			_ = json.NewDecoder(r.Body).Decode(&bodyMap)
			createdComments = append(createdComments, bodyMap["body"])
			commentMu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   newID,
				"body": bodyMap["body"],
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, fmt.Sprintf("/issues/%d/comments", prNum)):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/issues/comments/"):
			commentMu.Lock()
			var bodyMap map[string]string
			_ = json.NewDecoder(r.Body).Decode(&bodyMap)
			editedComments = append(editedComments, bodyMap["body"])
			commentMu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   1001,
				"body": bodyMap["body"],
			})
		case strings.Contains(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghTestClient, err := ghclient.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	cfg := &config.Config{
		WebhookSecret:        testSecret,
		WebhookStateDir:      stateDir,
		WebhookWorkers:       1,
		WebhookBacklog:       10,
		WebhookDeliveryTTL:   24 * time.Hour,
		WebhookDeliveryLimit: 100,
		WebhookStateMaxBytes: 16777216,
		WebhookBodyMaxBytes:  1048576,
		EffortLevel:          "lite",
		EnableSandbox:        false,
		LLMBaseURL:           llmServer.URL,
		LLMAPIKey:            "test-key",
		LLMModel:             "test-model",
		GitHubToken:          "test-token",
		AutoActions:          []string{"review"},
	}

	// 1. Setup mode creates no store and no workers
	setupCfg := &config.Config{
		GitHubAppSetupToken: "setup-token-123",
		PublicURL:           "https://example.com",
		WebhookStateDir:      filepath.Join(stateDir, "setup-sub"),
	}
	setupSrv := NewServer(setupCfg)
	if err := setupSrv.Start(context.Background()); err != nil {
		t.Fatalf("setup mode Start failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "setup-sub", "jobs.db")); !os.IsNotExist(err) {
		t.Fatalf("setup mode must not create jobs.db on filesystem")
	}

	// 2. Normal mode: signed HTTP tracer returns 200 only after DB commit
	srv := NewServer(cfg)
	srv.gh = ghTestClient
	llmTestClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghTestClient, llmTestClient, nil))
	// Don't start workers immediately to test DB commit before execution
	dbPath := filepath.Join(stateDir, "jobs.db")
	store, err := OpenJobStore(dbPath, StoreOptions{
		BacklogLimit:  10,
		DeliveryLimit: 100,
		StateMaxBytes: 16777216,
		DeliveryTTL:   24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	srv.SetStore(store)

	payloadJSON := fmt.Sprintf(`{
		"action": "opened",
		"pull_request": {
			"number": %d,
			"head": {"sha": %q, "ref": "feat-tracer"},
			"base": {"sha": %q, "ref": "main"}
		},
		"repository": {
			"id": %d,
			"name": "repo",
			"owner": {"login": "org"}
		}
	}`, prNum, validHeadSHA, validBaseSHA, repoID)
	payload := []byte(payloadJSON)

	req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Delivery", "delivery-tracer-001")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 accepted, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"accepted"`) {
		t.Fatalf("expected body to contain status accepted, got: %s", rr.Body.String())
	}

	// Assert delivery and job are durably committed in store
	deliv, err := store.GetDelivery(context.Background(), fmt.Sprintf("github.com/%d/delivery-tracer-001", repoID))
	if err != nil || deliv == nil {
		t.Fatalf("delivery was not committed to DB before 200 response: %v", err)
	}
	queuedJobs, err := store.ListQueuedJobs(context.Background())
	if err != nil || len(queuedJobs) != 1 {
		t.Fatalf("expected 1 queued job committed, got %d (err: %v)", len(queuedJobs), err)
	}

	// 3. Close and reopen store before execution retains the job and receipt
	if err := store.Close(); err != nil {
		t.Fatalf("failed to close store: %v", err)
	}

	reopenedStore, err := OpenJobStore(dbPath, StoreOptions{
		BacklogLimit:  10,
		DeliveryLimit: 100,
		StateMaxBytes: 16777216,
		DeliveryTTL:   24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("failed to reopen store: %v", err)
	}
	reopenedDeliv, err := reopenedStore.GetDelivery(context.Background(), fmt.Sprintf("github.com/%d/delivery-tracer-001", repoID))
	if err != nil || reopenedDeliv == nil {
		t.Fatalf("reopened store missing delivery receipt: %v", err)
	}
	reopenedJobs, err := reopenedStore.ListQueuedJobs(context.Background())
	if err != nil || len(reopenedJobs) != 1 {
		t.Fatalf("reopened store missing queued job: %v", err)
	}

	// 4. Start scheduler/workers to execute retained job
	srv.SetStore(reopenedStore)
	executor := NewServerJobExecutor(srv, reopenedStore)
	scheduler := NewScheduler(reopenedStore, executor, 1)
	srv.SetScheduler(scheduler)
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatalf("failed to start scheduler: %v", err)
	}

	// Wait for job to complete
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := reopenedStore.GetJob(context.Background(), reopenedJobs[0].ID)
		if err == nil && job != nil && job.Status == "completed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	completedJob, err := reopenedStore.GetJob(context.Background(), reopenedJobs[0].ID)
	if err != nil || completedJob.Status != "completed" {
		t.Fatalf("expected job status 'completed', got %v (err: %v)", completedJob, err)
	}

	// 5. Verify publication: status create ID patched for running/completed and separate review output
	commentMu.Lock()
	numCreated := len(createdComments)
	numEdited := len(editedComments)
	commentMu.Unlock()

	if numCreated != 2 {
		t.Fatalf("expected 2 created comments (1 status + 1 review output), got %d: %v", numCreated, createdComments)
	}
	if numEdited < 2 {
		t.Fatalf("expected status comment to be edited at least twice (running + completed), got %d: %v", numEdited, editedComments)
	}
	if atomic.LoadInt32(&llmCalls) != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", atomic.LoadInt32(&llmCalls))
	}
	if atomic.LoadInt32(&compareCalls) != 1 {
		t.Fatalf("expected exactly 1 compare call, got %d", atomic.LoadInt32(&compareCalls))
	}

	// 6. Same delivery returns duplicate and executes zero additional work
	reqDup := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
	reqDup.Header.Set("X-GitHub-Delivery", "delivery-tracer-001")
	reqDup.Header.Set("X-GitHub-Event", "pull_request")
	reqDup.Header.Set("Content-Type", "application/json")
	reqDup.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

	rrDup := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rrDup, reqDup)

	if rrDup.Code != http.StatusOK {
		t.Fatalf("expected 200 for duplicate, got %d", rrDup.Code)
	}
	if !strings.Contains(rrDup.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("expected body to contain status duplicate, got: %s", rrDup.Body.String())
	}
	if atomic.LoadInt32(&llmCalls) != 1 {
		t.Fatalf("duplicate delivery must make 0 additional LLM calls, got %d", atomic.LoadInt32(&llmCalls))
	}

	// Clean shutdown
	scheduler.Stop()
	_ = reopenedStore.Close()
}

