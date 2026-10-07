package server

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"go.etcd.io/bbolt"
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

func TestWebhookAdmissionBounds(t *testing.T) {
	const (
		testSecret = "bounds-test-secret"
		repoID     = int64(98765)
		prNum      = 42
		baseSHA    = "1111111111111111111111111111111111111111"
		headSHA    = "2222222222222222222222222222222222222222"
	)

	newTestServer := func(t *testing.T, backlogLimit int, bodyLimit int64) (*Server, *BoltJobStore, string) {
		stateDir := t.TempDir()
		cfg := &config.Config{
			WebhookSecret:        testSecret,
			WebhookStateDir:      stateDir,
			WebhookWorkers:       1,
			WebhookBacklog:       backlogLimit,
			WebhookDeliveryTTL:   24 * time.Hour,
			WebhookDeliveryLimit: 100,
			WebhookStateMaxBytes: 16777216,
			WebhookBodyMaxBytes:  bodyLimit,
			EffortLevel:          "lite",
			EnableSandbox:        false,
			LLMBaseURL:           "http://example.invalid",
			LLMAPIKey:            "key",
			LLMModel:             "model",
			GitHubToken:          "token",
			AutoActions:          []string{"review"},
		}
		dbPath := filepath.Join(stateDir, "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  backlogLimit,
			DeliveryLimit: 100,
			StateMaxBytes: 16777216,
			DeliveryTTL:   24 * time.Hour,
		})
		if err != nil {
			t.Fatalf("failed to open job store: %v", err)
		}
		srv := NewServer(cfg)
		srv.SetStore(store)
		return srv, store, stateDir
	}

	validPayload := func(rID int64, pNum int) []byte {
		return []byte(fmt.Sprintf(`{
			"action": "opened",
			"pull_request": {
				"number": %d,
				"head": {"sha": %q, "ref": "feat-x"},
				"base": {"sha": %q, "ref": "main"}
			},
			"repository": {
				"id": %d,
				"name": "repo",
				"owner": {"login": "org"}
			}
		}`, pNum, headSHA, baseSHA, rID))
	}

	t.Run("invalid delivery identifiers return 400 with no receipt", func(t *testing.T) {
		srv, store, _ := newTestServer(t, 10, 1048576)
		defer store.Close()

		testDeliveries := []string{
			"",                             // empty
			"invalid delivery with spaces", // spaces
			strings.Repeat("a", 129),       // > 128 chars
			"delivery\nwith\nnewlines",     // non-printable ASCII
		}

		payload := validPayload(repoID, prNum)
		for _, delivID := range testDeliveries {
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", delivID)
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("delivery %q: expected 400, got %d", delivID, rr.Code)
			}
		}

		// Verify zero receipts created
		jobs, _ := store.ListQueuedJobs(context.Background())
		if len(jobs) != 0 {
			t.Fatalf("expected 0 jobs admitted for invalid delivery IDs, got %d", len(jobs))
		}
	})

	t.Run("repository or PR zero returns 400 with no receipt", func(t *testing.T) {
		srv, store, _ := newTestServer(t, 10, 1048576)
		defer store.Close()

		cases := []struct {
			rID  int64
			pNum int
		}{
			{0, 10},
			{-1, 10},
			{repoID, 0},
			{repoID, -5},
		}

		for _, tc := range cases {
			payload := validPayload(tc.rID, tc.pNum)
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-zero-%d-%d", tc.rID, tc.pNum))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Errorf("rID=%d, pNum=%d: expected 400, got %d", tc.rID, tc.pNum, rr.Code)
			}
		}
	})

	t.Run("changed digest on same delivery ID returns 409 collision", func(t *testing.T) {
		srv, store, _ := newTestServer(t, 10, 1048576)
		defer store.Close()

		delivID := "deliv-collision-test"
		payloadA := validPayload(repoID, 1)
		payloadB := validPayload(repoID, 2)

		// First delivery succeeds
		reqA := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payloadA))
		reqA.Header.Set("X-GitHub-Delivery", delivID)
		reqA.Header.Set("X-GitHub-Event", "pull_request")
		reqA.Header.Set("Content-Type", "application/json")
		reqA.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payloadA))

		rrA := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rrA, reqA)
		if rrA.Code != http.StatusOK {
			t.Fatalf("first delivery expected 200, got %d", rrA.Code)
		}

		// Second delivery with same ID but different payload returns 409
		reqB := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payloadB))
		reqB.Header.Set("X-GitHub-Delivery", delivID)
		reqB.Header.Set("X-GitHub-Event", "pull_request")
		reqB.Header.Set("Content-Type", "application/json")
		reqB.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payloadB))

		rrB := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rrB, reqB)
		if rrB.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict, got %d: %s", rrB.Code, rrB.Body.String())
		}
	})

	t.Run("invalid signature returns 401", func(t *testing.T) {
		srv, store, _ := newTestServer(t, 10, 1048576)
		defer store.Close()

		payload := validPayload(repoID, prNum)
		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-sig-bad")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", "sha256=invalidbadhash")

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized, got %d", rr.Code)
		}
	})

	t.Run("body overflow returns 413", func(t *testing.T) {
		const smallLimit = 65536 // 64 KiB
		srv, store, _ := newTestServer(t, 10, smallLimit)
		defer store.Close()

		oversizedPayload := bytes.Repeat([]byte("A"), smallLimit+100)
		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(oversizedPayload))
		req.Header.Set("X-GitHub-Delivery", "deliv-oversized")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, oversizedPayload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413 Request Entity Too Large, got %d: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("full queue rejects atomically with 503 and Retry-After 30", func(t *testing.T) {
		const smallBacklog = 2
		srv, store, _ := newTestServer(t, smallBacklog, 1048576)
		defer store.Close()

		// Fill backlog
		for i := 1; i <= smallBacklog; i++ {
			p := validPayload(repoID, i)
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(p))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-fill-%d", i))
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, p))

			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("fill request %d expected 200, got %d", i, rr.Code)
			}
		}

		// Verify 2 queued jobs
		jobs, _ := store.ListQueuedJobs(context.Background())
		if len(jobs) != smallBacklog {
			t.Fatalf("expected %d jobs, got %d", smallBacklog, len(jobs))
		}

		// Next request exceeds backlog
		pOverflow := validPayload(repoID, smallBacklog+1)
		reqOverflow := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(pOverflow))
		reqOverflow.Header.Set("X-GitHub-Delivery", "deliv-overflow")
		reqOverflow.Header.Set("X-GitHub-Event", "pull_request")
		reqOverflow.Header.Set("Content-Type", "application/json")
		reqOverflow.Header.Set("X-Hub-Signature-256", signPayload(testSecret, pOverflow))

		rrOverflow := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rrOverflow, reqOverflow)

		if rrOverflow.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d: %s", rrOverflow.Code, rrOverflow.Body.String())
		}
		if rrOverflow.Header().Get("Retry-After") != "30" {
			t.Errorf("expected Retry-After header '30', got %q", rrOverflow.Header().Get("Retry-After"))
		}

		// Ensure no receipt created for rejected delivery
		_, err := store.GetDelivery(context.Background(), fmt.Sprintf("github.com/%d/deliv-overflow", repoID))
		if err == nil {
			t.Fatalf("receipt must not be created for rejected delivery")
		}
		// Existing jobs unchanged
		jobsAfter, _ := store.ListQueuedJobs(context.Background())
		if len(jobsAfter) != smallBacklog {
			t.Fatalf("existing queued jobs count altered, got %d, want %d", len(jobsAfter), smallBacklog)
		}
	})

	t.Run("concurrent identical deliveries commit exactly one job", func(t *testing.T) {
		srv, store, _ := newTestServer(t, 20, 1048576)
		defer store.Close()

		payload := validPayload(repoID, 100)
		delivID := "deliv-concurrent-001"
		sig := signPayload(testSecret, payload)

		const concurrency = 10
		var wg sync.WaitGroup
		statusCodes := make([]int, concurrency)

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
				req.Header.Set("X-GitHub-Delivery", delivID)
				req.Header.Set("X-GitHub-Event", "pull_request")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Hub-Signature-256", sig)

				rr := httptest.NewRecorder()
				srv.Routes().ServeHTTP(rr, req)
				statusCodes[idx] = rr.Code
			}(i)
		}
		wg.Wait()

		for i, code := range statusCodes {
			if code != http.StatusOK {
				t.Fatalf("request %d returned %d, want 200", i, code)
			}
		}

		// Exactly 1 job must be enqueued
		jobs, _ := store.ListQueuedJobs(context.Background())
		if len(jobs) != 1 {
			t.Fatalf("expected exactly 1 job admitted across %d concurrent deliveries, got %d", concurrency, len(jobs))
		}
	})

	t.Run("exclusive open failure and runtime unavailable truth in health", func(t *testing.T) {
		stateDir := t.TempDir()
		dbPath := filepath.Join(stateDir, "jobs.db")

		// Open first DB handle to hold exclusive lock
		db1, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
		if err != nil {
			t.Fatalf("failed to open primary bbolt DB: %v", err)
		}
		defer db1.Close()

		// Attempting second open should fail with ErrDatabaseLocked
		_, err = OpenJobStore(dbPath, StoreOptions{OpenTimeout: 50 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("expected database locked error, got: %v", err)
		}

		// Server with runtime error rejects actionable work with 503 and exposes degraded health
		srv := NewServer(&config.Config{
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
			EffortLevel:     "lite",
			EnableSandbox:   false,
			LLMBaseURL:      "http://example.invalid",
			LLMAPIKey:       "key",
			LLMModel:        "model",
			GitHubToken:     "token",
		})
		srv.SetRuntimeError(ErrDatabaseLocked)

		// Health reflects unavailable truthfully without secrets
		reqHealth := httptest.NewRequest("GET", "/healthz", nil)
		rrHealth := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rrHealth, reqHealth)

		if rrHealth.Code != http.StatusOK {
			t.Fatalf("health endpoint returned %d", rrHealth.Code)
		}
		var healthResp map[string]string
		_ = json.Unmarshal(rrHealth.Body.Bytes(), &healthResp)
		if healthResp["status"] != "degraded" || healthResp["runtime"] != "unavailable" {
			t.Fatalf("expected degraded/unavailable health, got: %v", healthResp)
		}
		if strings.Contains(rrHealth.Body.String(), testSecret) || strings.Contains(rrHealth.Body.String(), dbPath) {
			t.Fatalf("health response leaked secrets or internal paths")
		}

		// Actionable webhook returns 503 with Retry-After 30
		payload := validPayload(repoID, prNum)
		reqAction := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		reqAction.Header.Set("X-GitHub-Delivery", "deliv-locked")
		reqAction.Header.Set("X-GitHub-Event", "pull_request")
		reqAction.Header.Set("Content-Type", "application/json")
		reqAction.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rrAction := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rrAction, reqAction)
		if rrAction.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d", rrAction.Code)
		}
		if rrAction.Header().Get("Retry-After") != "30" {
			t.Errorf("expected Retry-After 30, got %q", rrAction.Header().Get("Retry-After"))
		}
	})

	t.Run("unknown schema version fails closed", func(t *testing.T) {
		stateDir := t.TempDir()
		dbPath := filepath.Join(stateDir, "corrupt.db")

		// Create a bolt db with unknown schema version 99
		db, err := bbolt.Open(dbPath, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(func(tx *bbolt.Tx) error {
			b, err := tx.CreateBucket([]byte("meta"))
			if err != nil {
				return err
			}
			verBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(verBytes, 99)
			return b.Put([]byte("schema_version"), verBytes)
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Close()

		_, err = OpenJobStore(dbPath, StoreOptions{})
		if err == nil || !strings.Contains(err.Error(), "unknown store schema version") {
			t.Fatalf("expected unknown schema version error, got: %v", err)
		}
	})
}


