package summarizer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

type fixtureTransport struct {
	allowedHost string
	rt          http.RoundTripper
}

func (f *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != f.allowedHost {
		return nil, fmt.Errorf("fixture transport rejected non-fixture URL host: %s", req.URL.Host)
	}
	base := f.rt
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func newFixtureClient(serverURL string) *http.Client {
	u, err := url.Parse(serverURL)
	if err != nil {
		panic(err)
	}
	return &http.Client{
		Transport: &fixtureTransport{
			allowedHost: u.Host,
		},
		Timeout: 5 * time.Second,
	}
}

func TestSummarizer_NoDiscussionShortcut(t *testing.T) {
	var llmCalls int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "Should not be called"}},
			},
		})
	}))
	defer llmServer.Close()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/10"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 10,
				"title":  "Add feature X",
				"body":   "Description of feature X",
				"user":   map[string]any{"login": "alice"},
			})
		case strings.Contains(r.URL.Path, "/issues/10/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/pulls/10/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create ghClient: %v", err)
	}

	cfg := &config.Config{
		LLMBaseURL: llmServer.URL,
		LLMAPIKey:  "key",
		LLMModel:   "model",
	}
	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	llmClient.SetHTTPClient(newFixtureClient(llmServer.URL))

	summ := NewSummarizerWithClients(cfg, ghClient, llmClient)

	summary, err := summ.SummarizeDiscussions(context.Background(), "owner", "repo", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedPrefix := "## 📝 PR Discussion Summary: #10 (Add feature X)"
	if !strings.HasPrefix(summary, expectedPrefix) {
		t.Errorf("expected summary to start with %q, got: %s", expectedPrefix, summary)
	}
	if !strings.Contains(summary, "No discussion comments or review threads have been posted yet.") {
		t.Errorf("expected no-discussion notice, got: %s", summary)
	}

	if calls := atomic.LoadInt32(&llmCalls); calls != 0 {
		t.Fatalf("expected 0 LLM calls for empty discussion, got %d", calls)
	}
}

func TestSummarizer_FullPromptAssemblyAndTrimmedOutput(t *testing.T) {
	var (
		llmCalls           int32
		capturedSysPrompt  string
		capturedUserPrompt string
	)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		var chatReq llm.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&chatReq)
		for _, m := range chatReq.Messages {
			if m.Role == "system" {
				capturedSysPrompt = m.Content
			} else if m.Role == "user" {
				capturedUserPrompt = m.Content
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: "  \n\n## 📝 PR Discussion & Review Summary: Add OAuth\n\nConsensus: LGTM\n\n  ",
					},
					FinishReason: "stop",
				},
			},
		})
	}))
	defer llmServer.Close()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/20"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 20,
				"title":  "Add OAuth",
				"body":   "Implements OAuth2 authentication.",
				"user":   map[string]any{"login": "alice"},
			})
		case strings.Contains(r.URL.Path, "/issues/20/comments"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "bob"}, "body": "Looks great overall."},
			})
		case strings.Contains(r.URL.Path, "/pulls/20/comments"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"path":      "auth/oauth.go",
					"line":      55,
					"diff_hunk": "@@ -50,6 +50,7 @@\n+func Authenticate() error",
					"user":      map[string]any{"login": "charlie"},
					"body":      "Please handle token expiry properly.",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create ghClient: %v", err)
	}

	cfg := &config.Config{
		LLMBaseURL: llmServer.URL,
		LLMAPIKey:  "key",
		LLMModel:   "model",
	}
	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	llmClient.SetHTTPClient(newFixtureClient(llmServer.URL))

	summ := NewSummarizerWithClients(cfg, ghClient, llmClient)

	summary, err := summ.SummarizeDiscussions(context.Background(), "owner", "repo", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls := atomic.LoadInt32(&llmCalls); calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", calls)
	}

	// Verify trimmed output
	expectedOutput := "## 📝 PR Discussion & Review Summary: Add OAuth\n\nConsensus: LGTM"
	if summary != expectedOutput {
		t.Errorf("expected trimmed summary %q, got %q", expectedOutput, summary)
	}

	// Verify system prompt
	if !strings.Contains(capturedSysPrompt, "elite Technical Program Manager") {
		t.Errorf("system prompt missing TPM persona")
	}

	// Verify user prompt contains PR title, author, description, general comment, and inline thread
	for _, expectedSubstr := range []string{
		"PR Title: Add OAuth",
		"Author: alice",
		"Implements OAuth2 authentication.",
		"[@bob]: Looks great overall.",
		"File `auth/oauth.go` (Line 55)",
		"func Authenticate() error",
		"[@charlie]: Please handle token expiry properly.",
	} {
		if !strings.Contains(capturedUserPrompt, expectedSubstr) {
			t.Errorf("user prompt missing expected element %q; full prompt:\n%s", expectedSubstr, capturedUserPrompt)
		}
	}
}

func TestSummarizer_FailuresZeroPostAndZeroLLM(t *testing.T) {
	var (
		llmCalls  int32
		postCalls int32
	)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		http.Error(w, "llm internal error", http.StatusInternalServerError)
	}))
	defer llmServer.Close()

	var (
		failPR       bool
		failComments bool
		failPost     bool
	)

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/100/comments") {
			atomic.AddInt32(&postCalls, 1)
			if failPost {
				http.Error(w, "github post error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 999}`))
			return
		}

		if strings.HasSuffix(r.URL.Path, "/pulls/100") {
			if failPR {
				http.Error(w, "github pr fetch error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 100,
				"title":  "Test PR",
				"body":   "Body",
				"user":   map[string]any{"login": "user"},
			})
			return
		}

		if strings.Contains(r.URL.Path, "/issues/100/comments") {
			if failComments {
				http.Error(w, "github comments fetch error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "bob"}, "body": "Discussion comment"},
			})
			return
		}

		if strings.Contains(r.URL.Path, "/pulls/100/comments") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}

		http.NotFound(w, r)
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create ghClient: %v", err)
	}

	cfg := &config.Config{
		LLMBaseURL: llmServer.URL,
		LLMAPIKey:  "key",
		LLMModel:   "model",
	}
	llmClient := llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)

	summ := NewSummarizerWithClients(cfg, ghClient, llmClient)

	// 1. PR fetch failure -> zero LLM calls, zero post calls
	failPR = true
	failComments = false
	failPost = false
	atomic.StoreInt32(&llmCalls, 0)
	atomic.StoreInt32(&postCalls, 0)

	_, err = summ.RunAndPost(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on PR fetch failure, got nil")
	}
	if calls := atomic.LoadInt32(&llmCalls); calls != 0 {
		t.Fatalf("expected 0 LLM calls on PR fetch failure, got %d", calls)
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 0 {
		t.Fatalf("expected 0 post calls on PR fetch failure, got %d", posts)
	}

	// 2. Comments fetch failure -> zero LLM calls, zero post calls
	failPR = false
	failComments = true
	atomic.StoreInt32(&llmCalls, 0)
	atomic.StoreInt32(&postCalls, 0)

	_, err = summ.RunAndPost(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on comments fetch failure, got nil")
	}
	if calls := atomic.LoadInt32(&llmCalls); calls != 0 {
		t.Fatalf("expected 0 LLM calls on comments fetch failure, got %d", calls)
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 0 {
		t.Fatalf("expected 0 post calls on comments fetch failure, got %d", posts)
	}

	// 3. LLM failure -> zero post calls
	failPR = false
	failComments = false
	atomic.StoreInt32(&llmCalls, 0)
	atomic.StoreInt32(&postCalls, 0)

	_, err = summ.RunAndPost(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on LLM failure, got nil")
	}
	if calls := atomic.LoadInt32(&llmCalls); calls != 1 {
		t.Fatalf("expected 1 LLM call on LLM failure, got %d", calls)
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 0 {
		t.Fatalf("expected 0 post calls on LLM failure, got %d", posts)
	}

	// 4. Posting failure -> error returned
	// Set LLM to return valid summary
	llmSuccessServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "Valid summary"}},
			},
		})
	}))
	defer llmSuccessServer.Close()

	llmSuccessClient := llm.NewClient(llmSuccessServer.URL, "key", "model")
	summWithSuccessLLM := NewSummarizerWithClients(cfg, ghClient, llmSuccessClient)

	failPost = true
	atomic.StoreInt32(&postCalls, 0)

	_, err = summWithSuccessLLM.RunAndPost(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on posting failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed to post summary to GitHub") {
		t.Fatalf("expected 'failed to post summary to GitHub' error, got: %v", err)
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 1 {
		t.Fatalf("expected 1 post call attempt, got %d", posts)
	}

	// 5. Blank LLM generation -> zero post calls
	llmBlankServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "   \n\t "}},
			},
		})
	}))
	defer llmBlankServer.Close()

	llmBlankClient := llm.NewClient(llmBlankServer.URL, "key", "model")
	summBlank := NewSummarizerWithClients(cfg, ghClient, llmBlankClient)

	failPost = false
	atomic.StoreInt32(&postCalls, 0)

	_, err = summBlank.RunAndPost(context.Background(), "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on blank LLM generation, got nil")
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 0 {
		t.Fatalf("expected 0 post calls on blank generation, got %d", posts)
	}

	// 6. Cancellation -> error returned, zero post calls
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	atomic.StoreInt32(&postCalls, 0)
	_, err = summWithSuccessLLM.RunAndPost(ctx, "owner", "repo", 100)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if posts := atomic.LoadInt32(&postCalls); posts != 0 {
		t.Fatalf("expected 0 post calls on cancelled context, got %d", posts)
	}
}
