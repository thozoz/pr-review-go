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

func TestAutomaticReviewCoalescing(t *testing.T) {
	const (
		testSecret = "coalesce-secret"
		repoID     = int64(8888)
		prNum      = 42
		baseSHA    = "1111111111111111111111111111111111111111"
		shaA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		shaC       = "cccccccccccccccccccccccccccccccccccccccc"
		shaD       = "dddddddddddddddddddddddddddddddddddddddd"
		shaE       = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)

	t.Run("block active review A, accept B C D, finish A without cancelling, post only D", func(t *testing.T) {
		var (
			currentHeadMu sync.Mutex
			currentHead   = shaA
			publishedHead = ""
			reportsPosted []string
			reportsMu     sync.Mutex
			blockAChan    = make(chan struct{})
			startedAChan  = make(chan struct{})
			startedAOnce  sync.Once
			activeJobsMu  sync.Mutex
			activeJobs    = 0
			maxActiveJobs = 0
		)

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			currentHeadMu.Lock()
			headAtReview := currentHead
			currentHeadMu.Unlock()

			if headAtReview == shaA {
				startedAOnce.Do(func() {
					close(startedAChan)
				})
				// Block active review A
				<-blockAChan
			}

			resp := llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Content: fmt.Sprintf(`{"score": 90, "summary": "Review for %s", "findings": []}`, headAtReview),
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
				currentHeadMu.Lock()
				head := currentHead
				currentHeadMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": prNum,
					"title":  "Coalescing PR",
					"body":   "Initial body",
					"head":   map[string]any{"sha": head, "ref": "feat-coalesce"},
					"base":   map[string]any{"sha": baseSHA, "ref": "main"},
					"user":   map[string]any{"login": "developer"},
				})
			case strings.Contains(r.URL.Path, "/compare/"):
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/f.go b/f.go\n+new line\n"))
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, fmt.Sprintf("/issues/%d/comments", prNum)):
				var bodyMap map[string]string
				_ = json.NewDecoder(r.Body).Decode(&bodyMap)
				body := bodyMap["body"]

				// Track published review report (not status comments)
				if strings.Contains(body, "Review for ") {
					reportsMu.Lock()
					reportsPosted = append(reportsPosted, body)
					if strings.Contains(body, shaD) {
						publishedHead = shaD
					}
					reportsMu.Unlock()
				}

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": body,
				})
			case strings.Contains(r.URL.Path, "/issues/comments/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": "edited",
				})
			case strings.Contains(r.URL.Path, "/comments"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, err := ghclient.NewTestClient(ghServer.URL)
		if err != nil {
			t.Fatal(err)
		}

		stateDir := t.TempDir()
		cfg := &config.Config{
			WebhookSecret:        testSecret,
			WebhookStateDir:      stateDir,
			WebhookWorkers:       2,
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

		srv := NewServer(cfg)
		srv.gh = ghClient
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghClient, llmClient, nil))

		dbPath := filepath.Join(stateDir, "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  10,
			DeliveryLimit: 100,
			StateMaxBytes: 16777216,
			DeliveryTTL:   24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		srv.SetStore(store)

		// Create a wrapped executor to observe concurrent PR executions
		baseExecutor := NewServerJobExecutor(srv, store)
		type trackingExecutor struct {
			JobExecutor
		}
		wrapExec := &trackingExecutor{
			JobExecutor: JobExecutorFunc(func(ctx context.Context, j *Job) error {
				activeJobsMu.Lock()
				activeJobs++
				if activeJobs > maxActiveJobs {
					maxActiveJobs = activeJobs
				}
				activeJobsMu.Unlock()

				err := baseExecutor.ExecuteJob(ctx, j)

				activeJobsMu.Lock()
				activeJobs--
				activeJobsMu.Unlock()
				return err
			}),
		}

		sched := NewScheduler(store, wrapExec, 2)
		srv.SetScheduler(sched)
		if err := sched.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer sched.Stop()

		postWebhook := func(delivID, action, head string) {
			payloadJSON := fmt.Sprintf(`{
				"action": %q,
				"pull_request": {
					"number": %d,
					"head": {"sha": %q, "ref": "feat-coalesce"},
					"base": {"sha": %q, "ref": "main"}
				},
				"repository": {
					"id": %d,
					"name": "repo",
					"owner": {"login": "org"}
				}
			}`, action, prNum, head, baseSHA, repoID)
			payload := []byte(payloadJSON)

			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", delivID)
			req.Header.Set("X-GitHub-Event", "pull_request")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("webhook %s failed: %d (%s)", delivID, rr.Code, rr.Body.String())
			}
		}

		// 1. Deliver A
		postWebhook("deliv-A", "opened", shaA)

		// Await review A reaching blocked state
		select {
		case <-startedAChan:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for review A to start")
		}

		// 2. While A is active/blocked, deliver B, C, D
		currentHeadMu.Lock()
		currentHead = shaB
		currentHeadMu.Unlock()
		postWebhook("deliv-B", "synchronize", shaB)

		currentHeadMu.Lock()
		currentHead = shaC
		currentHeadMu.Unlock()
		postWebhook("deliv-C", "synchronize", shaC)

		currentHeadMu.Lock()
		currentHead = shaD
		currentHeadMu.Unlock()
		postWebhook("deliv-D", "synchronize", shaD)

		// Assert that in store, only at most 1 queued successor review exists
		queued, err := store.ListQueuedJobs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(queued) != 1 {
			t.Fatalf("expected exactly 1 coalesced successor job queued, got %d", len(queued))
		}
		if queued[0].HeadSHA != shaD {
			t.Fatalf("expected queued successor to be head D (%s), got %s", shaD, queued[0].HeadSHA)
		}

		// 3. Unblock A
		close(blockAChan)

		// Await publication of D
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			reportsMu.Lock()
			done := (publishedHead == shaD)
			reportsMu.Unlock()
			if done {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		reportsMu.Lock()
		defer reportsMu.Unlock()
		if publishedHead != shaD {
			t.Fatalf("expected published report for %s, got: %v", shaD, reportsPosted)
		}

		// Verify no report for A was ever posted!
		for _, r := range reportsPosted {
			if strings.Contains(r, shaA) {
				t.Fatalf("report for superseded head A was published: %s", r)
			}
			if strings.Contains(r, shaB) || strings.Contains(r, shaC) {
				t.Fatalf("intermediate report for B or C was published: %s", r)
			}
		}

		// 4. Verify no two effective actions overlapped for one PR
		activeJobsMu.Lock()
		if maxActiveJobs > 1 {
			t.Fatalf("expected at most 1 active job per PR, observed %d", maxActiveJobs)
		}
		activeJobsMu.Unlock()
	})

	t.Run("full queue still retains an already-reserved successor", func(t *testing.T) {
		stateDir := t.TempDir()
		dbPath := filepath.Join(stateDir, "full_queue.db")
		// Backlog limit of 2
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  2,
			DeliveryLimit: 100,
			StateMaxBytes: 16777216,
			DeliveryTTL:   24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		prKey1, _ := MakePRKey("github.com", 101, 1)
		prKey2, _ := MakePRKey("github.com", 102, 2)
		prKey3, _ := MakePRKey("github.com", 103, 3)

		// 1. Admit automatic review for PR 1
		res1, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: 101, DeliveryID: "d-1", PayloadHash: "h1", ReceivedAt: time.Now(),
		}, []Job{
			{Kind: "review", Trigger: "automatic", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: shaA},
		})
		if err != nil || res1.Status != AdmitAccepted {
			t.Fatalf("admit PR1 failed: %v, status=%v", err, res1.Status)
		}

		// 2. Claim PR1: starts running and reserves 1 successor slot
		claimed1, err := store.ClaimNextJob(context.Background(), nil)
		if err != nil || claimed1 == nil {
			t.Fatalf("claim PR1 failed: %v", err)
		}
		prState1, _ := store.GetPRState(context.Background(), prKey1)
		if !prState1.HasReservedSuccessor || prState1.ActiveJobID == "" {
			t.Fatalf("PR1 should have reserved successor: %+v", prState1)
		}

		// 3. Admit job for PR 2: fills the 1 remaining backlog slot (1 queued + 1 reserved = 2 backlog)
		res2, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: 102, DeliveryID: "d-2", PayloadHash: "h2", ReceivedAt: time.Now(),
		}, []Job{
			{Kind: "labels", Trigger: "automatic", PRKey: prKey2, Owner: "o", Repo: "r", PRNumber: 2},
		})
		if err != nil || res2.Status != AdmitAccepted {
			t.Fatalf("admit PR2 failed: %v, status=%v", err, res2.Status)
		}

		// 4. PR1 sends update (head B): uses ALREADY-RESERVED successor slot, succeeds even when ordinary queue is full!
		res1Update, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: 101, DeliveryID: "d-1-up", PayloadHash: "h1-up", ReceivedAt: time.Now(),
		}, []Job{
			{Kind: "review", Trigger: "automatic", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: shaB},
		})
		if err != nil || res1Update.Status != AdmitAccepted {
			t.Fatalf("admit PR1 update using reserved slot failed: %v, status=%v, reason=%s", err, res1Update.Status, res1Update.Reason)
		}

		// 5. Try to admit job for unreserved PR 3: rejected with 503 capacity full
		res3, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: 103, DeliveryID: "d-3", PayloadHash: "h3", ReceivedAt: time.Now(),
		}, []Job{
			{Kind: "review", Trigger: "automatic", PRKey: prKey3, Owner: "o", Repo: "r", PRNumber: 3, HeadSHA: shaC},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res3.Status != AdmitCapacityFull {
			t.Fatalf("expected PR3 to be rejected for capacity full, got status=%v", res3.Status)
		}
	})

	t.Run("delayed delivery cannot revert latest GitHub head", func(t *testing.T) {
		var reviewedHeads []string
		var headsMu sync.Mutex

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Content: `{"score": 90, "summary": "Review", "findings": []}`,
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
					"title":  "Delayed test PR",
					"body":   "Initial body",
					"head":   map[string]any{"sha": shaD, "ref": "feat-d"},
					"base":   map[string]any{"sha": baseSHA, "ref": "main"},
					"user":   map[string]any{"login": "developer"},
				})
			case strings.Contains(r.URL.Path, "/compare/"):
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/f.go b/f.go\n+new line\n"))
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, fmt.Sprintf("/issues/%d/comments", prNum)):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": "comment",
				})
			case strings.Contains(r.URL.Path, "/issues/comments/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": "edited",
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, err := ghclient.NewTestClient(ghServer.URL)
		if err != nil {
			t.Fatal(err)
		}

		stateDir := t.TempDir()
		cfg := &config.Config{
			EffortLevel:   "lite",
			EnableSandbox: false,
			LLMBaseURL:    llmServer.URL,
			LLMAPIKey:     "test-key",
			LLMModel:      "test-model",
			GitHubToken:   "test-token",
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghClient, llmClient, nil))

		dbPath := filepath.Join(stateDir, "delayed.db")
		store, err := OpenJobStore(dbPath, StoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		srv.SetStore(store)

		prKey, _ := MakePRKey("github.com", repoID, prNum)

		// Set last reviewed head to shaD
		_ = store.UpdatePRState(context.Background(), &PRState{
			PRKey:            prKey,
			Owner:            "o",
			Repo:             "r",
			Number:           prNum,
			Generation:       5,
			LastReviewedHead: shaD,
		})

		// Job arrives with delayed old head A
		res, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: repoID, DeliveryID: "deliv-delayed-1", PayloadHash: "h-del", ReceivedAt: time.Now(),
		}, []Job{
			{
				Kind:       "review",
				Trigger:    "automatic",
				PRKey:      prKey,
				Owner:      "o",
				Repo:       "r",
				PRNumber:   prNum,
				HeadSHA:    shaA,
				BaseSHA:    baseSHA,
			},
		})
		if err != nil || res.Status != AdmitAccepted {
			t.Fatalf("admit failed: %v, status=%v", err, res.Status)
		}

		delayedJob, err := store.ClaimNextJob(context.Background(), nil)
		if err != nil || delayedJob == nil {
			t.Fatalf("claim failed: %v", err)
		}

		executor := NewServerJobExecutor(srv, store)
		err = executor.ExecuteJob(context.Background(), delayedJob)
		if err != nil {
			t.Fatalf("executor failed: %v", err)
		}

		// Verify that delayed job was superseded without re-reviewing or reverting D to A
		delayedJobAfter, _ := store.GetJob(context.Background(), delayedJob.ID)
		if delayedJobAfter.Status != "superseded" {
			t.Fatalf("expected delayed job to be superseded, got status %s", delayedJobAfter.Status)
		}

		headsMu.Lock()
		for _, h := range reviewedHeads {
			if h == shaA {
				t.Fatalf("delayed SHA A was reviewed: %s", h)
			}
		}
		headsMu.Unlock()
	})

	t.Run("head changes without delivered event still suppress stale output and retain latest-head intent", func(t *testing.T) {
		var (
			currentHeadMu sync.Mutex
			currentHead   = shaA
			postedReports []string
			reportsMu     sync.Mutex
		)

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate developer pushing head E while LLM is running
			currentHeadMu.Lock()
			currentHead = shaE
			currentHeadMu.Unlock()

			resp := llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Content: `{"score": 90, "summary": "Review for A", "findings": []}`,
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
				currentHeadMu.Lock()
				head := currentHead
				currentHeadMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": prNum,
					"title":  "Undelivered push PR",
					"body":   "Initial body",
					"head":   map[string]any{"sha": head, "ref": "feat-e"},
					"base":   map[string]any{"sha": baseSHA, "ref": "main"},
					"user":   map[string]any{"login": "developer"},
				})
			case strings.Contains(r.URL.Path, "/compare/"):
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/f.go b/f.go\n+new line\n"))
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, fmt.Sprintf("/issues/%d/comments", prNum)):
				var bodyMap map[string]string
				_ = json.NewDecoder(r.Body).Decode(&bodyMap)
				if strings.Contains(bodyMap["body"], "Review for") {
					reportsMu.Lock()
					postedReports = append(postedReports, bodyMap["body"])
					reportsMu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": bodyMap["body"],
				})
			case strings.Contains(r.URL.Path, "/issues/comments/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"body": "edited",
				})
			case strings.Contains(r.URL.Path, "/comments"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, err := ghclient.NewTestClient(ghServer.URL)
		if err != nil {
			t.Fatal(err)
		}

		stateDir := t.TempDir()
		cfg := &config.Config{
			EffortLevel:   "lite",
			EnableSandbox: false,
			LLMBaseURL:    llmServer.URL,
			LLMAPIKey:     "test-key",
			LLMModel:      "test-model",
			GitHubToken:   "test-token",
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghClient, llmClient, nil))

		dbPath := filepath.Join(stateDir, "undelivered.db")
		store, err := OpenJobStore(dbPath, StoreOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		srv.SetStore(store)

		prKey, _ := MakePRKey("github.com", repoID, prNum)
		res, err := store.Admit(context.Background(), Delivery{
			Host: "github.com", RepoID: repoID, DeliveryID: "deliv-a", PayloadHash: "ha", ReceivedAt: time.Now(),
		}, []Job{
			{
				Kind:       "review",
				Trigger:    "automatic",
				PRKey:      prKey,
				Owner:      "o",
				Repo:       "r",
				PRNumber:   prNum,
				HeadSHA:    shaA,
				BaseSHA:    baseSHA,
			},
		})
		if err != nil || res.Status != AdmitAccepted {
			t.Fatalf("admit failed: %v, status=%v", err, res.Status)
		}

		jobA, err := store.ClaimNextJob(context.Background(), nil)
		if err != nil || jobA == nil {
			t.Fatalf("claim failed: %v", err)
		}

		executor := NewServerJobExecutor(srv, store)
		err = executor.ExecuteJob(context.Background(), jobA)
		if err != nil {
			t.Fatalf("executor failed: %v", err)
		}

		// 1. Verify stale report A was suppressed
		reportsMu.Lock()
		if len(postedReports) != 0 {
			t.Fatalf("expected 0 published reports, got: %v", postedReports)
		}
		reportsMu.Unlock()

		jobAAfter, _ := store.GetJob(context.Background(), jobA.ID)
		if jobAAfter == nil || jobAAfter.Status != "superseded" {
			t.Fatalf("expected job A to be superseded, got %+v", jobAAfter)
		}

		// 2. Verify fresh successor review for E was scheduled in store
		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 1 {
			t.Fatalf("expected 1 successor job queued, got %d", len(queued))
		}
		if queued[0].HeadSHA != shaE {
			t.Fatalf("expected successor job to have head E (%s), got %s", shaE, queued[0].HeadSHA)
		}
	})
}

type JobExecutorFunc func(ctx context.Context, job *Job) error

func (f JobExecutorFunc) ExecuteJob(ctx context.Context, job *Job) error {
	return f(ctx, job)
}


