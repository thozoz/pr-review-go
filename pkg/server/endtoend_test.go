package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

type e2eFixtureTransport struct {
	allowedHosts map[string]bool
	rt           http.RoundTripper
}

func (f *e2eFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.allowedHosts[req.URL.Host] {
		return nil, fmt.Errorf("e2e fixture transport rejected non-fixture URL host: %s", req.URL.Host)
	}
	base := f.rt
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func newE2EFixtureClient(allowedURLs ...string) *http.Client {
	hosts := make(map[string]bool)
	for _, uStr := range allowedURLs {
		if u, err := url.Parse(uStr); err == nil {
			hosts[u.Host] = true
		}
	}
	return &http.Client{
		Transport: &e2eFixtureTransport{
			allowedHosts: hosts,
		},
		Timeout: 10 * time.Second,
	}
}

func makeGitHubHandler(
	headProvider func() string,
	postCommentCallback func(body string),
) http.HandlerFunc {
	var lastBody atomic.Value // last body written to the fake owned comment
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.Contains(path, "/collaborators/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "write"})
			return
		}
		if strings.Contains(path, "/compare/") {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n+new line\n"))
			return
		}

		// Comment endpoints: must be checked before /pulls/ to avoid matching /pulls/{n}/comments
		if r.Method == http.MethodPost && strings.Contains(path, "/comments") {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			lastBody.Store(b["body"])
			if postCommentCallback != nil {
				postCommentCallback(b["body"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "body": b["body"]})
			return
		}
		if (r.Method == http.MethodPatch || r.Method == http.MethodPost) && strings.Contains(path, "/issues/comments/") {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			lastBody.Store(b["body"])
			if postCommentCallback != nil {
				postCommentCallback(b["body"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "body": b["body"]})
			return
		}
		if r.Method == http.MethodGet && strings.Contains(path, "/issues/comments/") {
			body, _ := lastBody.Load().(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001, "body": body, "user": map[string]any{"id": 1001, "login": "test-bot"}})
			return
		}
		if strings.Contains(path, "/comments") {
			// Listing comments (pulls or issues) must return a JSON array
			_, _ = w.Write([]byte(`[]`))
			return
		}

		// Pull request details endpoint
		if strings.Contains(path, "/pulls/") {
			head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			if headProvider != nil {
				head = headProvider()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1,
				"title":  "Test PR",
				"body":   "Test PR description",
				"head":   map[string]any{"sha": head, "ref": "feat"},
				"base":   map[string]any{"sha": "1111111111111111111111111111111111111111", "ref": "main"},
				"user":   map[string]any{"login": "alice"},
			})
			return
		}

		http.NotFound(w, r)
	}
}

func makePROpenedPayload(repoID int64, prNum int, baseSHA, headSHA, author string) []byte {
	return []byte(fmt.Sprintf(`{
		"action": "opened",
		"number": %d,
		"pull_request": {
			"number": %d,
			"title": "PR #%d Title",
			"body": "PR description",
			"user": {"login": "%s"},
			"base": {"sha": "%s", "ref": "main"},
			"head": {"sha": "%s", "ref": "feature"}
		},
		"repository": {
			"id": %d,
			"name": "repo",
			"owner": {"login": "owner"}
		},
		"sender": {"login": "%s"}
	}`, prNum, prNum, prNum, author, baseSHA, headSHA, repoID, author))
}

func makePRSyncPayload(repoID int64, prNum int, baseSHA, headSHA, author string) []byte {
	return []byte(fmt.Sprintf(`{
		"action": "synchronize",
		"number": %d,
		"pull_request": {
			"number": %d,
			"title": "PR #%d Title",
			"body": "PR description",
			"user": {"login": "%s"},
			"base": {"sha": "%s", "ref": "main"},
			"head": {"sha": "%s", "ref": "feature"}
		},
		"repository": {
			"id": %d,
			"name": "repo",
			"owner": {"login": "owner"}
		},
		"sender": {"login": "%s"}
	}`, prNum, prNum, prNum, author, baseSHA, headSHA, repoID, author))
}

func makePRActionPayload(action string, repoID int64, prNum int, baseSHA, headSHA, author string) []byte {
	return []byte(fmt.Sprintf(`{
		"action": "%s",
		"number": %d,
		"pull_request": {
			"number": %d,
			"title": "PR #%d Title",
			"body": "PR description",
			"user": {"login": "%s"},
			"base": {"sha": "%s", "ref": "main"},
			"head": {"sha": "%s", "ref": "feature"}
		},
		"repository": {
			"id": %d,
			"name": "repo",
			"owner": {"login": "owner"}
		},
		"sender": {"login": "%s"}
	}`, action, prNum, prNum, prNum, author, baseSHA, headSHA, repoID, author))
}

func makeCommentPayload(repoID int64, prNum int, commentID int64, commentBody, author string) []byte {
	return []byte(fmt.Sprintf(`{
		"action": "created",
		"issue": {
			"number": %d,
			"pull_request": {"url": "https://api.github.com/repos/owner/repo/pulls/%d"}
		},
		"comment": {
			"id": %d,
			"body": "%s",
			"user": {"login": "%s"},
			"created_at": "2026-10-07T20:00:00Z"
		},
		"repository": {
			"id": %d,
			"name": "repo",
			"owner": {"login": "owner"}
		},
		"sender": {"login": "%s"}
	}`, prNum, prNum, commentID, commentBody, author, repoID, author))
}

func sendSignedWebhook(routes http.Handler, secret, event, deliveryID string, payload []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))

	rr := httptest.NewRecorder()
	routes.ServeHTTP(rr, req)
	return rr
}

func TestWebhookEndToEnd(t *testing.T) {
	const (
		secret       = "e2e-secret-key-12345"
		shaBase      = "1111111111111111111111111111111111111111"
		shaA         = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB         = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		shaC         = "cccccccccccccccccccccccccccccccccccccccc"
		shaD         = "dddddddddddddddddddddddddddddddddddddddd"
		repoID int64 = 99001
	)

	// 1. Scenario 1: Successful and Failing Review
	t.Run("SuccessfulAndFailingReview", func(t *testing.T) {
		var (
			llmCalls       int32
			reviewComments int32
			statusComments int32
			failLLM        int32
		)

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			if atomic.LoadInt32(&failLLM) == 1 {
				http.Error(w, "simulated llm error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Looks good", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, func(b string) {
			if strings.Contains(b, "pr-review-output") {
				atomic.AddInt32(&reviewComments, 1)
			}
			if strings.Contains(b, "pr-review-status") {
				atomic.AddInt32(&statusComments, 1)
			}
		}))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		llmClient.SetHTTPClient(newE2EFixtureClient(llmServer.URL))

		srv := NewServerWithClients(cfg, ghClient, llmClient)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		if err := srv.Start(ctx); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		defer srv.Shutdown(context.Background())

		// A. Success review on PR 1
		payload1 := makePROpenedPayload(repoID, 1, shaBase, shaA, "alice")
		rr1 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-success-1", payload1)
		if rr1.Code != http.StatusOK {
			t.Fatalf("expected 200 accepted, got %d: %s", rr1.Code, rr1.Body.String())
		}
		if !strings.Contains(rr1.Body.String(), `"status":"accepted"`) {
			t.Fatalf("expected status:accepted, got %s", rr1.Body.String())
		}

		// Wait for job execution
		for i := 0; i < 100; i++ {
			if atomic.LoadInt32(&llmCalls) >= 1 && atomic.LoadInt32(&reviewComments) >= 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if atomic.LoadInt32(&llmCalls) != 1 {
			t.Errorf("expected 1 LLM call, got %d", atomic.LoadInt32(&llmCalls))
		}
		if atomic.LoadInt32(&reviewComments) != 1 {
			t.Errorf("expected 1 review comment, got %d", atomic.LoadInt32(&reviewComments))
		}

		// B. Failing review on PR 2: LLM fails
		atomic.StoreInt32(&failLLM, 1)
		reviewCommentsBefore := atomic.LoadInt32(&reviewComments)
		statusCommentsBefore := atomic.LoadInt32(&statusComments)
		payload2 := makePROpenedPayload(repoID, 2, shaBase, shaA, "alice")
		rr2 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-fail-1", payload2)
		if rr2.Code != http.StatusOK {
			t.Fatalf("expected 200 accepted for fail review, got %d", rr2.Code)
		}

		for i := 0; i < 100; i++ {
			if atomic.LoadInt32(&llmCalls) >= 2 && atomic.LoadInt32(&statusComments) > statusCommentsBefore {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if atomic.LoadInt32(&llmCalls) != 2 {
			t.Fatalf("expected 2 total LLM calls, got %d", atomic.LoadInt32(&llmCalls))
		}
		// On LLM failure, status is updated, but no new review comment is posted
		if atomic.LoadInt32(&reviewComments) != reviewCommentsBefore {
			t.Errorf("expected zero new review comments on failure, got %d", atomic.LoadInt32(&reviewComments)-reviewCommentsBefore)
		}
	})

	// 2. Scenario 2: Identical Simultaneous Delivery
	t.Run("IdenticalSimultaneousDelivery", func(t *testing.T) {
		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Simultaneous OK", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, nil))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		payload := makePROpenedPayload(repoID, 20, shaBase, shaA, "alice")
		deliveryID := "simultaneous-delivery-unique-123"

		var wg sync.WaitGroup
		responses := make([]*httptest.ResponseRecorder, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				responses[idx] = sendSignedWebhook(srv.Routes(), secret, "pull_request", deliveryID, payload)
			}(i)
		}
		wg.Wait()

		acceptedCount := 0
		duplicateCount := 0
		for _, r := range responses {
			b := r.Body.String()
			if strings.Contains(b, `"status":"accepted"`) {
				acceptedCount++
			}
			if strings.Contains(b, `"status":"duplicate"`) {
				duplicateCount++
			}
		}

		if acceptedCount != 1 || duplicateCount != 1 {
			t.Fatalf("expected 1 accepted and 1 duplicate, got accepted=%d, duplicate=%d", acceptedCount, duplicateCount)
		}
	})

	// 3. Scenario 3: Close/Reopen Replay
	t.Run("CloseReopenReplay", func(t *testing.T) {
		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, nil))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
			WebhookWorkers:       1,
			WebhookBacklog:       10,
			WebhookDeliveryTTL:   24 * time.Hour,
			WebhookDeliveryLimit: 100,
			WebhookStateMaxBytes: 16777216,
			WebhookBodyMaxBytes:  1048576,
			EffortLevel:          "lite",
			EnableSandbox:        false,
			LLMBaseURL:           "http://localhost",
			LLMAPIKey:            "k",
			LLMModel:             "m",
			GitHubToken:          "t",
			AutoActions:          []string{"review"},
		}

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		// A. Closed event -> ignored
		closedPayload := makePRActionPayload("closed", repoID, 30, shaBase, shaA, "alice")
		rrClosed := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-closed-1", closedPayload)
		if rrClosed.Code != http.StatusOK || !strings.Contains(rrClosed.Body.String(), `"status":"ignored"`) {
			t.Fatalf("expected 200 ignored for closed event, got %d: %s", rrClosed.Code, rrClosed.Body.String())
		}

		// B. Reopened event -> admitted
		reopenedPayload := makePRActionPayload("reopened", repoID, 30, shaBase, shaA, "alice")
		rrReopened := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-reopened-1", reopenedPayload)
		if rrReopened.Code != http.StatusOK || !strings.Contains(rrReopened.Body.String(), `"status":"accepted"`) {
			t.Fatalf("expected 200 accepted for reopened event, got %d: %s", rrReopened.Code, rrReopened.Body.String())
		}

		// C. Exact same reopened event -> duplicate
		rrReopenedReplay := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-reopened-1", reopenedPayload)
		if rrReopenedReplay.Code != http.StatusOK || !strings.Contains(rrReopenedReplay.Body.String(), `"status":"duplicate"`) {
			t.Fatalf("expected 200 duplicate on replay, got %d: %s", rrReopenedReplay.Code, rrReopenedReplay.Body.String())
		}
	})

	// 4. Scenario 4: Queued-Status Running Edits (Status Recycling)
	t.Run("QueuedStatusRunningEdits", func(t *testing.T) {
		var (
			createdStatusComments int32
			editedStatusComments  int32
		)

		workerUnblock := make(chan struct{})

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-workerUnblock
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 95, "summary": "Done", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		var statusBody atomic.Value
		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.Path

			if strings.Contains(path, "/collaborators/") {
				_ = json.NewEncoder(w).Encode(map[string]any{"permission": "write"})
				return
			}
			if strings.Contains(path, "/compare/") {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n+1\n"))
				return
			}
			if r.Method == http.MethodPost && strings.Contains(path, "/issues/40/comments") {
				atomic.AddInt32(&createdStatusComments, 1)
				var b map[string]string
				_ = json.NewDecoder(r.Body).Decode(&b)
				statusBody.Store(b["body"])
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 4001, "body": b["body"]})
				return
			}
			if (r.Method == http.MethodPatch || r.Method == http.MethodPost) && strings.Contains(path, "/issues/comments/4001") {
				atomic.AddInt32(&editedStatusComments, 1)
				var b map[string]string
				_ = json.NewDecoder(r.Body).Decode(&b)
				statusBody.Store(b["body"])
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 4001, "body": b["body"]})
				return
			}
			if r.Method == http.MethodGet && strings.Contains(path, "/issues/comments/") {
				body, _ := statusBody.Load().(string)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 4001, "body": body, "user": map[string]any{"id": 1001, "login": "test-bot"}})
				return
			}
			if strings.Contains(path, "/comments") {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if strings.Contains(path, "/pulls/40") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 40, "title": "P40", "body": "B",
					"head": map[string]any{"sha": shaA, "ref": "f"},
					"base": map[string]any{"sha": shaBase, "ref": "main"},
					"user": map[string]any{"login": "alice"},
				})
				return
			}
			http.NotFound(w, r)
		}))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		payload := makePROpenedPayload(repoID, 40, shaBase, shaA, "alice")
		rr := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-status-1", payload)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		for i := 0; i < 50; i++ {
			if atomic.LoadInt32(&createdStatusComments) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		close(workerUnblock)

		for i := 0; i < 50; i++ {
			if atomic.LoadInt32(&editedStatusComments) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		if edits := atomic.LoadInt32(&editedStatusComments); edits < 1 {
			t.Errorf("expected status comment to be edited across transitions, got %d edits", edits)
		}
	})

	// 5. Scenario 5: B/C/D Coalescing During A
	t.Run("BCDCoalescingDuringA", func(t *testing.T) {
		var (
			llmCalls int32
			liveHead string = shaA
			liveMu   sync.Mutex
		)

		aRunning := make(chan struct{})
		aRelease := make(chan struct{})

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := atomic.AddInt32(&llmCalls, 1)
			if count == 1 {
				close(aRunning)
				<-aRelease
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Coalesced OK", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string {
			liveMu.Lock()
			defer liveMu.Unlock()
			return liveHead
		}, nil))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		// Start Review A
		payloadA := makePROpenedPayload(repoID, 50, shaBase, shaA, "alice")
		rrA := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-coalesce-A", payloadA)
		if rrA.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rrA.Code)
		}

		select {
		case <-aRunning:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for review A to start")
		}

		liveMu.Lock()
		liveHead = shaD
		liveMu.Unlock()

		payloadB := makePRSyncPayload(repoID, 50, shaBase, shaB, "alice")
		rrB := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-coalesce-B", payloadB)
		if rrB.Code != http.StatusOK {
			t.Fatalf("expected 200 for B, got %d", rrB.Code)
		}

		payloadC := makePRSyncPayload(repoID, 50, shaBase, shaC, "alice")
		rrC := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-coalesce-C", payloadC)
		if rrC.Code != http.StatusOK {
			t.Fatalf("expected 200 for C, got %d", rrC.Code)
		}

		payloadD := makePRSyncPayload(repoID, 50, shaBase, shaD, "alice")
		rrD := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-coalesce-D", payloadD)
		if rrD.Code != http.StatusOK {
			t.Fatalf("expected 200 for D, got %d", rrD.Code)
		}

		close(aRelease)

		for i := 0; i < 100; i++ {
			if atomic.LoadInt32(&llmCalls) >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		finalLLMCalls := atomic.LoadInt32(&llmCalls)
		if finalLLMCalls != 2 {
			t.Fatalf("expected exactly 2 LLM calls (A and D; B and C coalesced), got %d", finalLLMCalls)
		}
	})

	// 6. Scenario 6: Fresh Same-Head Explicit Rerun
	t.Run("FreshSameHeadExplicitRerun", func(t *testing.T) {
		var (
			llmCalls       int32
			statusComments []string
			mu             sync.Mutex
		)

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Rerun done", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, func(b string) {
			mu.Lock()
			statusComments = append(statusComments, b)
			mu.Unlock()
		}))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		// Initial review
		payloadInitial := makePROpenedPayload(repoID, 60, shaBase, shaA, "alice")
		rrInit := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-rerun-init", payloadInitial)
		if rrInit.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rrInit.Code)
		}

		// Wait until initial review completes and records reviewed head
		for i := 0; i < 100; i++ {
			mu.Lock()
			done := false
			for _, b := range statusComments {
				if strings.Contains(b, "Review completed") {
					done = true
					break
				}
			}
			mu.Unlock()
			if done && atomic.LoadInt32(&llmCalls) >= 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if atomic.LoadInt32(&llmCalls) < 1 {
			t.Fatalf("expected initial review to finish, got %d LLM calls", atomic.LoadInt32(&llmCalls))
		}

		// Fresh explicit /review comment on same head
		commentPayload := makeCommentPayload(repoID, 60, 7001, "/review", "alice")
		rrComment := sendSignedWebhook(srv.Routes(), secret, "issue_comment", "deliv-rerun-cmd", commentPayload)
		if rrComment.Code != http.StatusOK {
			t.Fatalf("expected 200 for rerun comment, got %d", rrComment.Code)
		}

		for i := 0; i < 150; i++ {
			if atomic.LoadInt32(&llmCalls) >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}

		if atomic.LoadInt32(&llmCalls) != 2 {
			t.Fatalf("expected 2 LLM calls for explicit rerun, got %d", atomic.LoadInt32(&llmCalls))
		}

		mu.Lock()
		foundRerunNotice := false
		for _, b := range statusComments {
			if strings.Contains(b, "This commit was already reviewed. Reviewing again.") {
				foundRerunNotice = true
				break
			}
		}
		mu.Unlock()

		if !foundRerunNotice {
			t.Errorf("expected status comment to contain 'This commit was already reviewed. Reviewing again.'")
		}
	})

	// 7. Scenario 7: Queue-Full Atomic Bundle (503 and no partial jobs)
	t.Run("QueueFullAtomicBundle", func(t *testing.T) {
		workerHold := make(chan struct{})

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-workerHold
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Done", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, nil))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
			WebhookWorkers:       1,
			WebhookBacklog:       2, // Backlog capacity of 2
			WebhookDeliveryTTL:   24 * time.Hour,
			WebhookDeliveryLimit: 100,
			WebhookStateMaxBytes: 16777216,
			WebhookBodyMaxBytes:  1048576,
			EffortLevel:          "lite",
			EnableSandbox:        false,
			LLMBaseURL:           llmServer.URL,
			LLMAPIKey:            "k",
			LLMModel:             "m",
			GitHubToken:          "t",
			AutoActions:          []string{"review"},
		}

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() {
			close(workerHold)
			srv.Shutdown(context.Background())
		}()

		// Job 1 is admitted and worker begins execution (held in LLM)
		p1 := makePROpenedPayload(repoID, 71, shaBase, shaA, "alice")
		r1 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-qf-1", p1)
		if r1.Code != http.StatusOK {
			t.Fatalf("expected 200 for job 1, got %d", r1.Code)
		}

		// Job 2 is admitted into queue backlog
		p2 := makePROpenedPayload(repoID, 72, shaBase, shaA, "alice")
		r2 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-qf-2", p2)
		if r2.Code != http.StatusOK {
			t.Fatalf("expected 200 for job 2, got %d", r2.Code)
		}

		// Job 3 is admitted into queue backlog
		p3 := makePROpenedPayload(repoID, 73, shaBase, shaA, "alice")
		r3 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-qf-3", p3)
		if r3.Code != http.StatusOK {
			t.Fatalf("expected 200 for job 3, got %d", r3.Code)
		}

		// Job 4 exceeds backlog -> 503 Service Unavailable with Retry-After: 30
		p4 := makePROpenedPayload(repoID, 74, shaBase, shaA, "alice")
		r4 := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-qf-4", p4)
		if r4.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 on saturated queue, got %d: %s", r4.Code, r4.Body.String())
		}
		if r4.Header().Get("Retry-After") != "30" {
			t.Errorf("expected Retry-After: 30 header on 503, got %q", r4.Header().Get("Retry-After"))
		}
	})

	// 8. Scenario 8: Stale Result / Remote-Write Race
	t.Run("StaleResultRemoteWriteRace", func(t *testing.T) {
		var (
			liveHead string = shaA
			liveMu   sync.Mutex
		)

		workerWait := make(chan struct{})
		var closeOnce sync.Once

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			liveMu.Lock()
			liveHead = shaB
			liveMu.Unlock()

			closeOnce.Do(func() {
				close(workerWait)
			})

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Done", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string {
			liveMu.Lock()
			defer liveMu.Unlock()
			return liveHead
		}, nil))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		payload := makePROpenedPayload(repoID, 80, shaBase, shaA, "alice")
		rr := sendSignedWebhook(srv.Routes(), secret, "pull_request", "deliv-stale-1", payload)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}

		select {
		case <-workerWait:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for worker")
		}

		time.Sleep(100 * time.Millisecond)
	})

	// 9. Scenario 9: Output-Commit Crash Recovery
	t.Run("OutputCommitCrashRecovery", func(t *testing.T) {
		stateDir := t.TempDir()
		var llmCalls int32
		var postCalls int32

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{
				Choices: []llm.ChatChoice{
					{Message: llm.ChatMessage{Role: "assistant", Content: `{"score": 90, "summary": "Crash intent OK", "findings": []}`}},
				},
			})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(makeGitHubHandler(func() string { return shaA }, func(b string) {
			atomic.AddInt32(&postCalls, 1)
		}))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)

		// Seed a job in store with stored output intent but interrupted before write
		storeOpts := StoreOptions{
			BacklogLimit:  10,
			DeliveryLimit: 100,
			StateMaxBytes: 16777216,
			DeliveryTTL:   24 * time.Hour,
		}
		store, err := OpenJobStore(filepath.Join(stateDir, "jobs.db"), storeOpts)
		if err != nil {
			t.Fatal(err)
		}

		prKey := PRKey{Host: "github.com", RepoID: repoID, Number: 90}
		rawIntent := `{"HeadSHA":"` + shaA + `","RawMarkdown":"## Review Report\nLooks good"}`
		job := Job{
			ID:           "job-crash-intent-9",
			Sequence:     1,
			PRKey:        prKey,
			Owner:        "owner",
			Repo:         "repo",
			PRNumber:     90,
			Kind:         "review",
			HeadSHA:      shaA,
			Status:       "running",
			OutputMarker: "intent-crash-9",
		}
		intent := OutputIntent{
			Marker:    "intent-crash-9",
			JobID:     job.ID,
			Action:    "review",
			PRKey:     prKey,
			Owner:     "owner",
			Repo:      "repo",
			PRNumber:  90,
			ExactHead: shaA,
			Body:      rawIntent,
			Status:    "pending",
		}

		delivery := Delivery{
			Host:        "github.com",
			RepoID:      repoID,
			DeliveryID:  "deliv-crash-9",
			EventKind:   "pull_request",
			PayloadHash: "hash9",
			ReceivedAt:  time.Now(),
		}

		if _, err := store.Admit(context.Background(), delivery, []Job{job}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveOutputIntent(context.Background(), &intent); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()

		// Start server - RecoverJobs will run on start and recover the output intent
		srv := NewServerWithClients(cfg, ghClient, llmClient)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		for i := 0; i < 50; i++ {
			if atomic.LoadInt32(&postCalls) >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		if atomic.LoadInt32(&llmCalls) != 0 {
			t.Errorf("expected 0 LLM calls during output-commit recovery, got %d", atomic.LoadInt32(&llmCalls))
		}
	})

	// 10. Scenario 10: Permission Failure
	t.Run("PermissionFailure", func(t *testing.T) {
		var llmCalls int32

		llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&llmCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(llm.ChatResponse{})
		}))
		defer llmServer.Close()

		ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/collaborators/"):
				http.NotFound(w, r)
			default:
				http.NotFound(w, r)
			}
		}))
		defer ghServer.Close()

		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
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

		ghClient, _ := ghclient.NewTestClient(ghServer.URL)
		llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		srv := NewServerWithClients(cfg, ghClient, llmClient)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer srv.Shutdown(context.Background())

		// Unauthorized user sends comment /review
		commentPayload := makeCommentPayload(repoID, 100, 8001, "/review", "unauthorized-user")
		rr := sendSignedWebhook(srv.Routes(), secret, "issue_comment", "deliv-unauth-1", commentPayload)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"ignored"`) {
			t.Fatalf("expected 200 ignored for unauthorized comment, got %d: %s", rr.Code, rr.Body.String())
		}

		if atomic.LoadInt32(&llmCalls) != 0 {
			t.Errorf("expected 0 LLM calls for unauthorized user, got %d", atomic.LoadInt32(&llmCalls))
		}
	})

	// 11. Scenario 11: Cancellation
	t.Run("Cancellation", func(t *testing.T) {
		cfg := &config.Config{
			Port:                 3000,
			WebhookSecret:        secret,
			WebhookStateDir:      t.TempDir(),
			WebhookWorkers:       1,
			WebhookBacklog:       10,
			WebhookDeliveryTTL:   24 * time.Hour,
			WebhookDeliveryLimit: 100,
			WebhookStateMaxBytes: 16777216,
			WebhookBodyMaxBytes:  1048576,
			EffortLevel:          "lite",
			EnableSandbox:        false,
			LLMBaseURL:           "http://localhost",
			LLMAPIKey:            "test-key",
			LLMModel:             "test-model",
			GitHubToken:          "test-token",
			AutoActions:          []string{"review"},
		}

		srv := NewServer(cfg)
		ctx, cancel := context.WithCancel(context.Background())

		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}

		// Cancel context while running
		cancel()

		// Shutdown should drain cleanly and not hang
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()

		srv.Shutdown(shutdownCtx)
	})
}
