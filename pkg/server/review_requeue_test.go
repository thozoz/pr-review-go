// Regression test for the live review-requeue defect reported in issue #20:
// while an explicit review generates output, admitting any other command for
// the same PR advances prState.Generation. The discarded explicit review was
// requeued WITHOUT refreshing job.Generation, so the retry discarded its own
// fresh output again (genChanged stays true) even with headChanged=false —
// looping forever and never publishing. The fix refreshes job.Generation to
// the current PR generation on explicit requeue.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/reviewer"
)

func TestExplicitReviewRequeueRefreshesGeneration(t *testing.T) {
	const (
		secret  = "requeue-secret"
		prNum   = 7
		baseSHA = "1111111111111111111111111111111111111111"
		headSHA = "2222222222222222222222222222222222222222"
	)

	var (
		reportsMu     sync.Mutex
		reportsPosted []string
		llmStarted    = make(chan struct{})
		llmBlock      = make(chan struct{})
		llmOnce       sync.Once
	)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmOnce.Do(func() { close(llmStarted) })
		<-llmBlock
		resp := llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Content: fmt.Sprintf(`{"score": 90, "summary": "Review for %s", "findings": []}`, headSHA),
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
				"title":  "Requeue PR",
				"body":   "Initial body",
				"head":   map[string]any{"sha": headSHA, "ref": "feat-requeue"},
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
			if strings.Contains(body, "Review for ") {
				reportsMu.Lock()
				reportsPosted = append(reportsPosted, body)
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
		WebhookSecret:        secret,
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

	store, err := OpenJobStore(stateDir+"/requeue.db", StoreOptions{
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

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 1, prNum)

	// 1. Admit explicit review at generation 1 (Author empty skips reauth).
	reviewJobs := []Job{{
		Kind: "review", Trigger: "explicit",
		PRKey: prKey, Owner: "o", Repo: "r", PRNumber: prNum,
		HeadSHA: headSHA, BaseSHA: baseSHA,
	}}
	if res, err := store.Admit(ctx, Delivery{Host: "github.com", RepoID: 1, DeliveryID: "d-review", PayloadHash: "h1", ReceivedAt: time.Now()}, reviewJobs); err != nil || res.Status != AdmitAccepted {
		t.Fatalf("admit review failed: %v, status=%v", err, res.Status)
	}
	reviewJob := reviewJobs[0]
	if reviewJob.Generation != 1 {
		t.Fatalf("expected review admitted at generation 1, got %d", reviewJob.Generation)
	}

	exec := NewServerJobExecutor(srv, store)

	// 2. Start generation; it blocks inside the LLM call.
	firstDone := make(chan error, 1)
	go func() { firstDone <- exec.executeReviewJob(ctx, &reviewJob) }()

	select {
	case <-llmStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for review generation to start")
	}

	// 3. While generating, another command lands on the same PR (same head):
	// prState.Generation advances to 2 without any head change.
	labelJobs := []Job{{
		Kind: "labels", Trigger: "explicit",
		PRKey: prKey, Owner: "o", Repo: "r", PRNumber: prNum,
		HeadSHA: headSHA, BaseSHA: baseSHA,
	}}
	if res, err := store.Admit(ctx, Delivery{Host: "github.com", RepoID: 1, DeliveryID: "d-labels", PayloadHash: "h2", ReceivedAt: time.Now()}, labelJobs); err != nil || res.Status != AdmitAccepted {
		t.Fatalf("admit labels failed: %v, status=%v", err, res.Status)
	}

	// 4. Let generation finish: output is discarded (stale generation) and
	// the explicit review is requeued.
	close(llmBlock)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first review run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first review run")
	}

	reportsMu.Lock()
	if len(reportsPosted) != 0 {
		reportsMu.Unlock()
		t.Fatalf("stale output must be discarded, got %d posted reports", len(reportsPosted))
	}
	reportsMu.Unlock()

	stored, err := store.GetJob(ctx, reviewJob.ID)
	if err != nil || stored == nil {
		t.Fatalf("requeued review job not found: %v", err)
	}
	if stored.Status != "queued" {
		t.Fatalf("expected requeued status, got %q", stored.Status)
	}
	if stored.Generation != 2 {
		t.Fatalf("requeued explicit review must refresh to current generation 2, got %d (infinite discard loop)", stored.Generation)
	}

	// 5. Retry publishes exactly one report for the unchanged head.
	if err := exec.executeReviewJob(ctx, stored); err != nil {
		t.Fatalf("second review run returned error: %v", err)
	}

	reportsMu.Lock()
	defer reportsMu.Unlock()
	if len(reportsPosted) != 1 {
		t.Fatalf("expected exactly 1 published report after requeue, got %d", len(reportsPosted))
	}
	if !strings.Contains(reportsPosted[0], headSHA) {
		t.Fatalf("published report is not for head %s: %s", headSHA, reportsPosted[0])
	}
}
