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
	"github.com/thozoz/pr-review-go/pkg/sandbox"
	"go.etcd.io/bbolt"
)

const testSecret = "tracer-webhook-secret"

func TestWebhookDurableTracer(t *testing.T) {
	const (
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
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/comments/"):
			commentMu.Lock()
			last := ""
			if n := len(createdComments); n > 0 {
				last = createdComments[n-1]
			}
			if n := len(editedComments); n > 0 {
				last = editedComments[n-1]
			}
			commentMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "body": last, "user": map[string]any{"id": 1001, "login": "test-bot"}})
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
		defer store.Close()
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

func TestCommandScheduling(t *testing.T) {
	t.Run("fairness between PRs and FIFO order retention", func(t *testing.T) {
		stateDir := t.TempDir()
		dbPath := filepath.Join(stateDir, "jobs.db")
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  20,
			DeliveryLimit: 100,
			StateMaxBytes: 16777216,
			DeliveryTTL:   24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		ctx := context.Background()
		prKey1, _ := MakePRKey("github.com", 100, 1)
		prKey2, _ := MakePRKey("github.com", 100, 2)

		deliv1 := Delivery{Host: "github.com", RepoID: 100, DeliveryID: "d1", EventKind: "issue_comment", PayloadHash: "h1", ReceivedAt: time.Now()}
		deliv2 := Delivery{Host: "github.com", RepoID: 100, DeliveryID: "d2", EventKind: "issue_comment", PayloadHash: "h2", ReceivedAt: time.Now()}

		res1, err := store.Admit(ctx, deliv1, []Job{
			{Kind: "describe", Trigger: "explicit", Status: "queued", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1},
			{Kind: "summary", Trigger: "explicit", Status: "queued", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1},
			{Kind: "labels", Trigger: "explicit", Status: "queued", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1},
		})
		if err != nil || res1.Status != AdmitAccepted {
			t.Fatalf("failed admitting PR 1 jobs: %v, status: %v", err, res1.Status)
		}

		res2, err := store.Admit(ctx, deliv2, []Job{
			{Kind: "review", Trigger: "explicit", Status: "queued", PRKey: prKey2, Owner: "o", Repo: "r", PRNumber: 2},
		})
		if err != nil || res2.Status != AdmitAccepted {
			t.Fatalf("failed admitting PR 2 job: %v, status: %v", err, res2.Status)
		}

		// 1. Claim first job: should be PR 1's first job ("describe")
		activePRs := make(map[string]bool)
		job1, err := store.ClaimNextJob(ctx, activePRs)
		if err != nil || job1 == nil {
			t.Fatalf("failed to claim job 1: %v", err)
		}
		if job1.Kind != "describe" || job1.PRNumber != 1 {
			t.Fatalf("expected PR 1 'describe', got PR %d '%s'", job1.PRNumber, job1.Kind)
		}
		activePRs[job1.PRKey.String()] = true

		// 2. While PR 1 is active, ClaimNextJob MUST pick PR 2's job ("review")
		// Other eligible PR advances while busy PR retains FIFO commands
		job2, err := store.ClaimNextJob(ctx, activePRs)
		if err != nil || job2 == nil {
			t.Fatalf("failed to claim job 2: %v", err)
		}
		if job2.Kind != "review" || job2.PRNumber != 2 {
			t.Fatalf("expected other eligible PR 2 'review', got PR %d '%s'", job2.PRNumber, job2.Kind)
		}
		activePRs[job2.PRKey.String()] = true

		// 3. Both PR 1 and PR 2 are busy; no other jobs should be claimable
		jobNone, err := store.ClaimNextJob(ctx, activePRs)
		if err != nil {
			t.Fatal(err)
		}
		if jobNone != nil {
			t.Fatalf("expected no claimable job while both PRs busy, got %+v", jobNone)
		}

		// 4. Release PR 1: next claim should get PR 1's second FIFO job ("summary")
		delete(activePRs, job1.PRKey.String())
		_ = store.ReleasePR(ctx, job1.PRKey)

		job3, err := store.ClaimNextJob(ctx, activePRs)
		if err != nil || job3 == nil {
			t.Fatalf("failed to claim job 3: %v", err)
		}
		if job3.Kind != "summary" || job3.PRNumber != 1 {
			t.Fatalf("expected PR 1 'summary' in FIFO sequence, got PR %d '%s'", job3.PRNumber, job3.Kind)
		}
		activePRs[job3.PRKey.String()] = true

		// 5. Release PR 1 again: next claim should get PR 1's third FIFO job ("labels")
		delete(activePRs, job3.PRKey.String())
		_ = store.ReleasePR(ctx, job3.PRKey)

		job4, err := store.ClaimNextJob(ctx, activePRs)
		if err != nil || job4 == nil {
			t.Fatalf("failed to claim job 4: %v", err)
		}
		if job4.Kind != "labels" || job4.PRNumber != 1 {
			t.Fatalf("expected PR 1 'labels' in FIFO sequence, got PR %d '%s'", job4.PRNumber, job4.Kind)
		}
	})

	t.Run("all recognized aliases admitted with correct kinds", func(t *testing.T) {
		const validHeadSHA = "2222222222222222222222222222222222222222"
		const validBaseSHA = "1111111111111111111111111111111111111111"

		aliases := []struct {
			command     string
			wantKind    string
			wantPayload string
		}{
			{"/review", "review", ""},
			{"/describe", "describe", ""},
			{"/update_changelog", "changelog", ""},
			{"/generate_labels", "labels", ""},
			{"/labels", "labels", ""},
			{"/summarize", "summary", ""},
			{"/summary", "summary", ""},
			{"/add_docs", "docs", ""},
			{"/docs", "docs", ""},
			{"@bot explain how this works", "assistant", "explain how this works"},
			{"@pr-review explain this", "assistant", "explain this"},
			{"/ask what is this function", "assistant", "what is this function"},
			{"/improve", "improve", ""},
		}

		for i, tc := range aliases {
			t.Run(tc.command, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.Contains(r.URL.Path, "/collaborators/"):
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
					case strings.Contains(r.URL.Path, "/pulls/"):
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{
							"number": 1,
							"head":   map[string]any{"sha": validHeadSHA, "ref": "feat"},
							"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
						})
					default:
						http.NotFound(w, r)
					}
				}))
				defer api.Close()

				ghClient, err := ghclient.NewTestClient(api.URL)
				if err != nil {
					t.Fatal(err)
				}

				stateDir := t.TempDir()
				cfg := &config.Config{
					GitHubToken:     "test-token",
					LLMAPIKey:       "test-key",
					LLMModel:        "test-model",
					LLMBaseURL:      "http://example.invalid",
					WebhookSecret:   testSecret,
					WebhookStateDir: stateDir,
				}
				srv := NewServer(cfg)
				srv.gh = ghClient
				defer srv.Stop()

				store, err := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{
					BacklogLimit:  100,
					DeliveryLimit: 100,
					StateMaxBytes: 16777216,
					DeliveryTTL:   24 * time.Hour,
				})
				if err != nil {
					t.Fatal(err)
				}
				srv.SetStore(store)

				commentID := int64(3000 + i)
				payloadJSON := fmt.Sprintf(`{
					"action": "created",
					"issue": {
						"number": 1,
						"pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/1"}
					},
					"comment": {
						"id": %d,
						"body": %q,
						"user": {"login": "dev"}
					},
					"repository": {
						"id": 12345,
						"name": "repo",
						"owner": {"login": "org"}
					}
				}`, commentID, tc.command)
				payload := []byte(payloadJSON)

				req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
				req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-alias-%d", i))
				req.Header.Set("X-GitHub-Event", "issue_comment")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

				rr := httptest.NewRecorder()
				srv.Routes().ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("command %q returned status %d: %s", tc.command, rr.Code, rr.Body.String())
				}

				queued, err := store.ListQueuedJobs(context.Background())
				if err != nil || len(queued) != 1 {
					t.Fatalf("command %q expected 1 queued job, got %d (err=%v)", tc.command, len(queued), err)
				}
				if queued[0].Kind != tc.wantKind {
					t.Errorf("command %q expected kind %q, got %q", tc.command, tc.wantKind, queued[0].Kind)
				}
				if queued[0].Payload != tc.wantPayload {
					t.Errorf("command %q expected payload %q, got %q", tc.command, tc.wantPayload, queued[0].Payload)
				}
				if queued[0].Trigger != "explicit" {
					t.Errorf("command %q expected trigger 'explicit', got %q", tc.command, queued[0].Trigger)
				}
			})
		}
	})

	t.Run("command argument text bounded to 4096 bytes", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
		}))
		defer api.Close()

		ghClient, _ := ghclient.NewTestClient(api.URL)
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      "http://example.invalid",
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		longQuestion := strings.Repeat("z", 5000)
		body := "/ask " + longQuestion
		payloadJSON := fmt.Sprintf(`{
			"action": "created",
			"issue": {"number": 1, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/1"}},
			"comment": {"id": 9999, "body": %q, "user": {"login": "dev"}},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`, body)
		payload := []byte(payloadJSON)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-bounded")
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 1 {
			t.Fatalf("expected 1 queued job, got %d", len(queued))
		}
		if len(queued[0].Payload) > 4096 {
			t.Fatalf("expected payload bounded to <= 4096 bytes, got %d", len(queued[0].Payload))
		}
	})
}

func TestAutomaticActions(t *testing.T) {
	const validHeadSHA = "2222222222222222222222222222222222222222"
	const validBaseSHA = "1111111111111111111111111111111111111111"

	t.Run("opened with empty body selects labels, describe, and review; improves skipped", func(t *testing.T) {
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      "http://example.invalid",
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
			AutoActions:     []string{"labels", "describe", "improve", "review"},
		}
		srv := NewServer(cfg)
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		payloadJSON := fmt.Sprintf(`{
			"action": "opened",
			"pull_request": {
				"number": 1,
				"body": "",
				"head": {"sha": %q, "ref": "feat"},
				"base": {"sha": %q, "ref": "main"}
			},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`, validHeadSHA, validBaseSHA)
		payload := []byte(payloadJSON)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-opened-empty")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 3 {
			t.Fatalf("expected 3 jobs (labels, describe, review), got %d: %+v", len(queued), queued)
		}

		kinds := []string{queued[0].Kind, queued[1].Kind, queued[2].Kind}
		expectedKinds := []string{"labels", "describe", "review"}
		for i, exp := range expectedKinds {
			if kinds[i] != exp {
				t.Errorf("job %d expected kind %q, got %q", i, exp, kinds[i])
			}
			if queued[i].Trigger != "automatic" {
				t.Errorf("job %d expected trigger automatic, got %q", i, queued[i].Trigger)
			}
		}
	})

	t.Run("opened with non-empty body selects labels and review only", func(t *testing.T) {
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      "http://example.invalid",
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
			AutoActions:     []string{"labels", "describe", "improve", "review"},
		}
		srv := NewServer(cfg)
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		payloadJSON := fmt.Sprintf(`{
			"action": "opened",
			"pull_request": {
				"number": 2,
				"body": "Existing detailed PR description",
				"head": {"sha": %q, "ref": "feat"},
				"base": {"sha": %q, "ref": "main"}
			},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`, validHeadSHA, validBaseSHA)
		payload := []byte(payloadJSON)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-opened-desc")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 2 {
			t.Fatalf("expected 2 jobs (labels, review), got %d: %+v", len(queued), queued)
		}
		if queued[0].Kind != "labels" || queued[1].Kind != "review" {
			t.Errorf("expected [labels, review], got [%s, %s]", queued[0].Kind, queued[1].Kind)
		}
	})

	t.Run("synchronize and reopened select review only", func(t *testing.T) {
		for _, action := range []string{"synchronize", "reopened"} {
			t.Run(action, func(t *testing.T) {
				stateDir := t.TempDir()
				cfg := &config.Config{
					GitHubToken:     "test-token",
					LLMAPIKey:       "test-key",
					LLMModel:        "test-model",
					LLMBaseURL:      "http://example.invalid",
					WebhookSecret:   testSecret,
					WebhookStateDir: stateDir,
					AutoActions:     []string{"labels", "describe", "improve", "review"},
				}
				srv := NewServer(cfg)
				defer srv.Stop()

				store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
				srv.SetStore(store)

				payloadJSON := fmt.Sprintf(`{
					"action": %q,
					"pull_request": {
						"number": 3,
						"body": "",
						"head": {"sha": %q, "ref": "feat"},
						"base": {"sha": %q, "ref": "main"}
					},
					"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
				}`, action, validHeadSHA, validBaseSHA)
				payload := []byte(payloadJSON)

				req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
				req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-%s", action))
				req.Header.Set("X-GitHub-Event", "pull_request")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

				rr := httptest.NewRecorder()
				srv.Routes().ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d", rr.Code)
				}

				queued, _ := store.ListQueuedJobs(context.Background())
				if len(queued) != 1 || queued[0].Kind != "review" {
					t.Fatalf("expected 1 'review' job for %s, got: %+v", action, queued)
				}
			})
		}
	})
}

func TestWebhookCommandReplay(t *testing.T) {
	const validHeadSHA = "2222222222222222222222222222222222222222"
	const validBaseSHA = "1111111111111111111111111111111111111111"

	t.Run("distinct authorized comment IDs run independently", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/collaborators/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
			case strings.Contains(r.URL.Path, "/pulls/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 1,
					"head":   map[string]any{"sha": validHeadSHA, "ref": "feat"},
					"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer api.Close()

		ghClient, _ := ghclient.NewTestClient(api.URL)
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      "http://example.invalid",
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		for _, commentID := range []int64{5001, 5002} {
			payloadJSON := fmt.Sprintf(`{
				"action": "created",
				"issue": {"number": 1, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/1"}},
				"comment": {"id": %d, "body": "/review", "user": {"login": "dev"}},
				"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
			}`, commentID)
			payload := []byte(payloadJSON)

			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-c-%d", commentID))
			req.Header.Set("X-GitHub-Event", "issue_comment")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), `"status":"accepted"`) {
				t.Fatalf("expected accepted, got %s", rr.Body.String())
			}
		}

		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 2 {
			t.Fatalf("expected 2 jobs for distinct comment IDs, got %d", len(queued))
		}
	})

	t.Run("duplicate comment ID and duplicate delivery replay suppressed", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/collaborators/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
			case strings.Contains(r.URL.Path, "/pulls/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 1,
					"head":   map[string]any{"sha": validHeadSHA, "ref": "feat"},
					"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer api.Close()

		ghClient, _ := ghclient.NewTestClient(api.URL)
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      "http://example.invalid",
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		commentID := int64(6001)
		payloadJSON := fmt.Sprintf(`{
			"action": "created",
			"issue": {"number": 1, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/1"}},
			"comment": {"id": %d, "body": "/review", "user": {"login": "dev"}},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`, commentID)
		payload := []byte(payloadJSON)

		// 1. Initial delivery: accepted
		req1 := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req1.Header.Set("X-GitHub-Delivery", "deliv-dup-1")
		req1.Header.Set("X-GitHub-Event", "issue_comment")
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr1 := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr1, req1)
		if rr1.Code != http.StatusOK || !strings.Contains(rr1.Body.String(), `"status":"accepted"`) {
			t.Fatalf("expected accepted, got %d: %s", rr1.Code, rr1.Body.String())
		}

		// 2. Replay with identical delivery ID: duplicate
		req2 := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req2.Header.Set("X-GitHub-Delivery", "deliv-dup-1")
		req2.Header.Set("X-GitHub-Event", "issue_comment")
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr2 := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusOK || !strings.Contains(rr2.Body.String(), `"status":"duplicate"`) {
			t.Fatalf("expected duplicate for delivery ID replay, got %d: %s", rr2.Code, rr2.Body.String())
		}

		// 3. Replay with NEW delivery ID but SAME comment ID: duplicate
		req3 := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req3.Header.Set("X-GitHub-Delivery", "deliv-dup-2")
		req3.Header.Set("X-GitHub-Event", "issue_comment")
		req3.Header.Set("Content-Type", "application/json")
		req3.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr3 := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr3, req3)
		if rr3.Code != http.StatusOK || !strings.Contains(rr3.Body.String(), `"status":"duplicate"`) {
			t.Fatalf("expected duplicate for comment ID replay, got %d: %s", rr3.Code, rr3.Body.String())
		}

		// Verify only 1 job in store
		queued, _ := store.ListQueuedJobs(context.Background())
		if len(queued) != 1 {
			t.Fatalf("expected exactly 1 job in store, got %d", len(queued))
		}
	})

	t.Run("fresh /review rerun on already reviewed head shows exact notice and reruns LLM", func(t *testing.T) {
		var llmCalls int32
		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message:      llm.ChatMessage{Content: `{"score": 95, "summary": "Rerun review passed", "findings": []}`},
						FinishReason: "stop",
					},
				},
			})
		}))
		defer llmServer.Close()

		var createdComments []string
		var commentsMu sync.Mutex
		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/collaborators/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
			case strings.HasSuffix(r.URL.Path, "/pulls/10"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 10,
					"head":   map[string]any{"sha": validHeadSHA, "ref": "feat"},
					"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
				})
			case strings.Contains(r.URL.Path, "/compare/"):
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/main.go b/main.go\n+func Rerun() {}\n"))
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/10/comments"):
				var bodyMap map[string]string
				_ = json.NewDecoder(r.Body).Decode(&bodyMap)
				commentsMu.Lock()
				createdComments = append(createdComments, bodyMap["body"])
				commentsMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 8888, "body": bodyMap["body"]})
			case strings.Contains(r.URL.Path, "/comments"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      llmServer.URL,
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
			EffortLevel:     "lite",
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghClient, llmClient, nil))
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		prKey, _ := MakePRKey("github.com", 12345, 10)

		// Set PRState showing validHeadSHA was already reviewed
		_ = store.UpdatePRState(context.Background(), &PRState{
			PRKey:            prKey,
			Generation:       1,
			LastReviewedHead: validHeadSHA,
		})

		// Send fresh /review command
		payloadJSON := fmt.Sprintf(`{
			"action": "created",
			"issue": {"number": 10, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/10"}},
			"comment": {"id": 7001, "body": "/review", "user": {"login": "dev"}},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`)
		payload := []byte(payloadJSON)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-rerun-1")
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		job, err := store.ClaimNextJob(context.Background(), nil)
		if err != nil || job == nil {
			t.Fatalf("expected claimable rerun job, got: %v", err)
		}

		executor := NewServerJobExecutor(srv, store)
		err = executor.ExecuteJob(context.Background(), job)
		if err != nil {
			t.Fatalf("ExecuteJob failed: %v", err)
		}

		// Verify LLM was actually called for rerun
		if atomic.LoadInt32(&llmCalls) != 1 {
			t.Fatalf("expected 1 LLM call for rerun, got %d", atomic.LoadInt32(&llmCalls))
		}

		// Verify exact rerun notice displayed in created comments
		commentsMu.Lock()
		defer commentsMu.Unlock()
		foundNotice := false
		const wantNotice = "This commit was already reviewed. Reviewing again."
		for _, c := range createdComments {
			if strings.Contains(c, wantNotice) {
				foundNotice = true
				break
			}
		}
		if !foundNotice {
			t.Fatalf("expected comments to contain %q, but got: %v", wantNotice, createdComments)
		}
	})

	t.Run("permission revocation/denial fails closed with zero effective action", func(t *testing.T) {
		var llmCalls int32
		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{})
		}))
		defer llmServer.Close()

		var permAllowed atomic.Bool
		permAllowed.Store(true) // allowed at admission

		var commentsCreated int32
		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/collaborators/"):
				w.Header().Set("Content-Type", "application/json")
				if permAllowed.Load() {
					_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]string{"permission": "read"})
				}
			case strings.HasSuffix(r.URL.Path, "/pulls/20"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 20,
					"head":   map[string]any{"sha": validHeadSHA, "ref": "feat"},
					"base":   map[string]any{"sha": validBaseSHA, "ref": "main"},
				})
			case strings.Contains(r.URL.Path, "/comments"):
				atomic.AddInt32(&commentsCreated, 1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 999})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		stateDir := t.TempDir()
		cfg := &config.Config{
			GitHubToken:     "test-token",
			LLMAPIKey:       "test-key",
			LLMModel:        "test-model",
			LLMBaseURL:      llmServer.URL,
			WebhookSecret:   testSecret,
			WebhookStateDir: stateDir,
		}
		srv := NewServer(cfg)
		srv.gh = ghClient
		defer srv.Stop()

		store, _ := OpenJobStore(filepath.Join(stateDir, "jobs.db"), StoreOptions{BacklogLimit: 10, DeliveryLimit: 10, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour})
		srv.SetStore(store)

		// 1. Admit job when permission is write
		payloadJSON := fmt.Sprintf(`{
			"action": "created",
			"issue": {"number": 20, "pull_request": {"url": "https://api.github.com/repos/org/repo/pulls/20"}},
			"comment": {"id": 8001, "body": "/review", "user": {"login": "dev-revoked"}},
			"repository": {"id": 12345, "name": "repo", "owner": {"login": "org"}}
		}`)
		payload := []byte(payloadJSON)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Delivery", "deliv-revoked-1")
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(testSecret, payload))

		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		job, err := store.ClaimNextJob(context.Background(), nil)
		if err != nil || job == nil {
			t.Fatalf("expected job to be claimed, got: %v", err)
		}

		// 2. Revoke permission before execution
		permAllowed.Store(false)

		// 3. Execute job
		executor := NewServerJobExecutor(srv, store)
		err = executor.ExecuteJob(context.Background(), job)
		if err == nil {
			t.Fatal("expected ExecuteJob to fail for revoked actor, but got nil")
		}

		// 4. Assert zero effective actions performed
		if atomic.LoadInt32(&llmCalls) != 0 {
			t.Fatalf("expected 0 LLM calls for revoked actor, got %d", atomic.LoadInt32(&llmCalls))
		}
		if atomic.LoadInt32(&commentsCreated) != 0 {
			t.Fatalf("expected 0 comments created for revoked actor, got %d", atomic.LoadInt32(&commentsCreated))
		}

		// Check job status in store is failed
		jobAfter, _ := store.GetJob(context.Background(), job.ID)
		if jobAfter == nil || jobAfter.Status != "failed" {
			t.Fatalf("expected job status 'failed', got %+v", jobAfter)
		}
		if !strings.Contains(jobAfter.Error, "permission revoked") {
			t.Fatalf("expected error mentioning permission revoked, got: %s", jobAfter.Error)
		}
	})
}

func TestWebhookCapacityConfig(t *testing.T) {
	t.Run("defaults and environment loading", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "token")
		t.Setenv("LLM_API_KEY", "key")
		t.Setenv("LLM_MODEL", "model")
		t.Setenv("LLM_BASE_URL", "http://example.com")
		t.Setenv("LLM_CONCURRENCY", "4")
		t.Setenv("LLM_MIN_INTERVAL", "500ms")
		t.Setenv("LLM_RESPONSE_MAX_BYTES", "2097152")
		t.Setenv("SANDBOX_CONCURRENCY", "3")

		cfg := config.Load()
		if cfg.LLMConcurrency != 4 {
			t.Fatalf("expected LLMConcurrency 4, got %d", cfg.LLMConcurrency)
		}
		if cfg.LLMMinInterval != 500*time.Millisecond {
			t.Fatalf("expected LLMMinInterval 500ms, got %v", cfg.LLMMinInterval)
		}
		if cfg.LLMResponseMaxBytes != 2097152 {
			t.Fatalf("expected LLMResponseMaxBytes 2097152, got %d", cfg.LLMResponseMaxBytes)
		}
		if cfg.SandboxConcurrency != 3 {
			t.Fatalf("expected SandboxConcurrency 3, got %d", cfg.SandboxConcurrency)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config validation failed: %v", err)
		}
	})

	t.Run("validation rejects negative zero overflow values", func(t *testing.T) {
		baseCfg := func() *config.Config {
			return &config.Config{
				GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u",
				EffortLevel: "lite", EnableSandbox: false,
			}
		}

		invalidCases := []struct {
			name   string
			mutate func(c *config.Config)
		}{
			{"negative llm concurrency", func(c *config.Config) { c.LLMConcurrency = -1 }},
			{"overflow llm concurrency", func(c *config.Config) { c.LLMConcurrency = 17 }},
			{"negative min interval", func(c *config.Config) { c.LLMMinInterval = -1 * time.Second }},
			{"underflow min interval", func(c *config.Config) { c.LLMMinInterval = 100 * time.Microsecond }},
			{"overflow min interval", func(c *config.Config) { c.LLMMinInterval = 2 * time.Minute }},
			{"negative max bytes", func(c *config.Config) { c.LLMResponseMaxBytes = -1 }},
			{"underflow max bytes", func(c *config.Config) { c.LLMResponseMaxBytes = 100 }},
			{"overflow max bytes", func(c *config.Config) { c.LLMResponseMaxBytes = 20 * 1024 * 1024 }},
			{"negative sandbox concurrency", func(c *config.Config) { c.SandboxConcurrency = -1 }},
			{"overflow sandbox concurrency", func(c *config.Config) { c.SandboxConcurrency = 17 }},
		}

		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				c := baseCfg()
				tc.mutate(c)
				if err := c.Validate(); err == nil {
					t.Fatalf("expected validation error for %s, got nil", tc.name)
				}
			})
		}
	})
}

func TestSharedActionCapacity(t *testing.T) {
	t.Run("distinct actions share active concurrency and start rate limits", func(t *testing.T) {
		var activeMu sync.Mutex
		var currentActive int64
		var maxActive int64
		var totalCalls int64

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&totalCalls, 1)
			activeMu.Lock()
			currentActive++
			if currentActive > maxActive {
				maxActive = currentActive
			}
			activeMu.Unlock()

			time.Sleep(30 * time.Millisecond)

			activeMu.Lock()
			currentActive--
			activeMu.Unlock()

			resp := llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{
						Message: llm.ChatMessage{
							Content: `{"score": 85, "summary": "action complete", "findings": [], "labels": ["bug"]}`,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer llmServer.Close()

		gate, err := llm.NewRequestGate(2, 5*time.Millisecond, 1048576, nil)
		if err != nil {
			t.Fatalf("gate creation failed: %v", err)
		}

		client1 := llm.NewClient(llmServer.URL, "key", "model")
		client2 := llm.NewClient(llmServer.URL, "key", "model")
		client3 := llm.NewClient(llmServer.URL, "key", "model")

		clients := []*llm.Client{client1, client2, client3}
		var wg sync.WaitGroup

		ctx := llm.WithAdmission(context.Background(), gate)

		// Run 6 concurrent requests across 3 distinct clients
		for i := 0; i < 6; i++ {
			wg.Add(1)
			clientIdx := i % len(clients)
			go func(c *llm.Client) {
				defer wg.Done()
				_, reqErr := c.ChatCompletion(ctx, "system", "user")
				if reqErr != nil {
					t.Errorf("request failed: %v", reqErr)
				}
			}(clients[clientIdx])
		}

		wg.Wait()

		if atomic.LoadInt64(&totalCalls) != 6 {
			t.Fatalf("expected 6 total calls, got %d", atomic.LoadInt64(&totalCalls))
		}

		activeMu.Lock()
		observedMax := maxActive
		activeMu.Unlock()

		if observedMax > 2 {
			t.Fatalf("shared gate exceeded concurrency limit: max active was %d (expected <= 2)", observedMax)
		}
	})
}

func TestWorkerFairness(t *testing.T) {
	t.Run("independent worker snapshot and llm limits with post-verification cleanup", func(t *testing.T) {
		stateDir := t.TempDir()
		store, err := OpenJobStore(filepath.Join(stateDir, "fairness.db"), StoreOptions{
			BacklogLimit: 20, DeliveryLimit: 100, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		var (
			workersActive      int64
			maxWorkersActive   int64
			snapshotsActive    int64
			maxSnapshotsActive int64
			llmActive          int64
			maxLLMActive       int64
			mu                 sync.Mutex
		)

		updateMax := func(val int64, max *int64) {
			mu.Lock()
			if val > *max {
				*max = val
			}
			mu.Unlock()
		}

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cur := atomic.AddInt64(&llmActive, 1)
			updateMax(cur, &maxLLMActive)
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&llmActive, -1)

			resp := llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Review", "findings": []}`}},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/pulls/"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 1,
					"title":  "Test PR",
					"head":   map[string]any{"sha": "1111111111111111111111111111111111111111", "ref": "feat"},
					"base":   map[string]any{"sha": "2222222222222222222222222222222222222222", "ref": "main"},
					"user":   map[string]any{"login": "dev"},
				})
			case strings.Contains(r.URL.Path, "/compare/"):
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n+new line\n"))
			case strings.Contains(r.URL.Path, "/comments"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 100})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		cfg := &config.Config{
			WebhookWorkers:      2,
			SandboxConcurrency:  1,
			LLMConcurrency:      2,
			LLMMinInterval:      1 * time.Millisecond,
			LLMResponseMaxBytes: 1048576,
			EffortLevel:         "balanced",
			EnableSandbox:       true,
			LLMBaseURL:          llmServer.URL,
			LLMAPIKey:           "key",
			LLMModel:            "model",
			GitHubToken:         "token",
		}

		srv := NewServer(cfg)
		srv.gh = ghClient
		srv.SetStore(store)

		runner := sandbox.NewRunner(1 * time.Minute)
		runner.Config = cfg
		mockSP := &mockSourceProviderWithTracker{
			onPrep: func() {
				cur := atomic.AddInt64(&snapshotsActive, 1)
				updateMax(cur, &maxSnapshotsActive)
			},
			onClean: func() {
				atomic.AddInt64(&snapshotsActive, -1)
			},
		}
		runner.SetSourceProvider(mockSP)

		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv.SetEngine(reviewer.NewEngineWithClients(cfg, ghClient, llmClient, runner))

		baseExec := NewServerJobExecutor(srv, store)
		trackingExec := JobExecutorFunc(func(ctx context.Context, job *Job) error {
			cur := atomic.AddInt64(&workersActive, 1)
			updateMax(cur, &maxWorkersActive)
			err := baseExec.ExecuteJob(ctx, job)
			atomic.AddInt64(&workersActive, -1)
			return err
		})

		sched := NewScheduler(store, trackingExec, 2)
		llmGate, _ := llm.NewRequestGate(cfg.LLMConcurrency, cfg.LLMMinInterval, cfg.LLMResponseMaxBytes, nil)
		sandboxGate, _ := sandbox.NewAdmissionGate(cfg.SandboxConcurrency)
		sched.SetLLMGate(llmGate)
		sched.SetSandboxGate(sandboxGate)

		if err := sched.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer sched.Stop()

		prKey1, _ := MakePRKey("github.com", 1, 1)
		prKey2, _ := MakePRKey("github.com", 1, 2)

		_, _ = store.Admit(context.Background(), Delivery{Host: "github.com", RepoID: 1, DeliveryID: "d1", PayloadHash: "h1", ReceivedAt: time.Now()}, []Job{
			{Kind: "review", Trigger: "automatic", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "1111111111111111111111111111111111111111"},
		})
		_, _ = store.Admit(context.Background(), Delivery{Host: "github.com", RepoID: 1, DeliveryID: "d2", PayloadHash: "h2", ReceivedAt: time.Now()}, []Job{
			{Kind: "review", Trigger: "automatic", PRKey: prKey2, Owner: "o", Repo: "r", PRNumber: 2, HeadSHA: "1111111111111111111111111111111111111111"},
		})

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			queued, _ := store.ListQueuedJobs(context.Background())
			if len(queued) == 0 && atomic.LoadInt64(&workersActive) == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}

		mu.Lock()
		maxSnap := maxSnapshotsActive
		maxWk := maxWorkersActive
		mu.Unlock()

		if maxSnap > 1 {
			t.Fatalf("expected snapshot concurrency <= 1, observed %d", maxSnap)
		}
		if maxWk == 0 {
			t.Fatalf("expected workers to run, but maxWorkersActive was 0")
		}
	})

	t.Run("fair progress with blocked PR and old accepted jobs retained", func(t *testing.T) {
		stateDir := t.TempDir()
		store, err := OpenJobStore(filepath.Join(stateDir, "blocked_pr.db"), StoreOptions{
			BacklogLimit: 20, DeliveryLimit: 100, StateMaxBytes: 16777216, DeliveryTTL: 24 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		ctx := context.Background()
		prKey1, _ := MakePRKey("github.com", 10, 1)
		prKey2, _ := MakePRKey("github.com", 10, 2)

		_ = store.UpdatePRState(ctx, &PRState{
			PRKey:            prKey1,
			Owner:            "o",
			Repo:             "r",
			Number:           1,
			HasBlockedAction: true,
		})

		_, _ = store.Admit(ctx, Delivery{Host: "github.com", RepoID: 10, DeliveryID: "d-block", PayloadHash: "hb", ReceivedAt: time.Now()}, []Job{
			{Kind: "review", Trigger: "automatic", Status: "queued", PRKey: prKey1, Owner: "o", Repo: "r", PRNumber: 1},
		})
		_, _ = store.Admit(ctx, Delivery{Host: "github.com", RepoID: 10, DeliveryID: "d-fair", PayloadHash: "hf", ReceivedAt: time.Now()}, []Job{
			{Kind: "review", Trigger: "automatic", Status: "queued", PRKey: prKey2, Owner: "o", Repo: "r", PRNumber: 2},
		})

		claimed, err := store.ClaimNextJob(ctx, nil)
		if err != nil || claimed == nil {
			t.Fatalf("expected eligible PR 2 to be claimed, got: %v", err)
		}
		if claimed.PRNumber != 2 {
			t.Fatalf("expected PR 2 to be claimed while PR 1 is blocked, got PR %d", claimed.PRNumber)
		}

		queued, _ := store.ListQueuedJobs(ctx)
		if len(queued) != 1 || queued[0].PRNumber != 1 {
			t.Fatalf("expected PR 1 job to remain queued without being discarded, got: %+v", queued)
		}
	})
}

type mockSourceProviderWithTracker struct {
	onPrep  func()
	onClean func()
}

func (m *mockSourceProviderWithTracker) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*sandbox.Snapshot, func(), error) {
	if m.onPrep != nil {
		m.onPrep()
	}
	snap := &sandbox.Snapshot{
		CommitSHA: headSHA,
		SourceDir: "/tmp/mock-tracked",
	}
	cleanup := func() {
		if m.onClean != nil {
			m.onClean()
		}
	}
	return snap, cleanup, nil
}




