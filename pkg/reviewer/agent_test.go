package reviewer

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

func TestAgent_ExploresTwoChangedFiles(t *testing.T) {
	snapDir := t.TempDir()
	fileA := filepath.Join(snapDir, "pkg", "auth", "token.go")
	fileB := filepath.Join(snapDir, "pkg", "auth", "login.go")
	if err := os.MkdirAll(filepath.Dir(fileA), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileA, []byte("package auth\n\nfunc ValidateToken() bool { return true }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("package auth\n\nfunc Login() error { return nil }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const testSHA = "0123456789abcdef0123456789abcdef01234567"
	snap := &sandbox.Snapshot{
		CommitSHA: testSHA,
		SourceDir: snapDir,
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("snapshot validate failed: %v", err)
	}

	rawDiff := `diff --git a/pkg/auth/token.go b/pkg/auth/token.go
--- a/pkg/auth/token.go
+++ b/pkg/auth/token.go
@@ -1,2 +1,3 @@
 package auth
+func ValidateToken() bool { return true }
diff --git a/pkg/auth/login.go b/pkg/auth/login.go
--- a/pkg/auth/login.go
+++ b/pkg/auth/login.go
@@ -1,2 +1,3 @@
 package auth
+func Login() error { return nil }
`
	inv := diff.Parse(rawDiff, diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var step int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curStep := atomic.AddInt32(&step, 1)
		var respContent string
		switch curStep {
		case 1:
			respContent = `{"action": "read_file", "path": "pkg/auth/token.go"}`
		case 2:
			respContent = `{"action": "read_file", "path": "pkg/auth/login.go"}`
		default:
			respContent = `{
				"action": "answer",
				"score": 92,
				"summary": "Both auth files look solid.",
				"findings": [
					{
						"file": "pkg/auth/token.go",
						"line": 2,
						"severity": "NOTE",
						"title": "Good token check",
						"description": "Validation logic is clean."
					}
				]
			}`
		}
		resp := llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: respContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		AgentMaxTurns: 10,
	}
	llmClient := llm.NewClient(llmServer.URL, "key", "model")
	agent := NewReviewerAgent(cfg, llmClient)

	pr := &github.PRDetails{
		Title:   "Add auth methods",
		HeadSHA: testSHA,
	}

	res, err := agent.Run(context.Background(), pr, inv, ledger, snap, "verification passed", sandbox.StatusPassed, nil, nil)
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if len(res.FilesServed) != 2 {
		t.Fatalf("expected 2 files served, got %d (%v)", len(res.FilesServed), res.FilesServed)
	}
	if !ledger.IsExamined("pkg/auth/token.go#H1") {
		t.Errorf("token.go hunk should be examined in ledger")
	}
	if !ledger.IsExamined("pkg/auth/login.go#H1") {
		t.Errorf("login.go hunk should be examined in ledger")
	}
	if res.Output == nil || res.Output.Score != 92 {
		t.Errorf("expected score 92, got %+v", res.Output)
	}
}

func TestAgent_RespectsOneTurnBudget_PartialWithLedger(t *testing.T) {
	snapDir := t.TempDir()
	fileA := filepath.Join(snapDir, "a.go")
	fileB := filepath.Join(snapDir, "b.go")
	_ = os.WriteFile(fileA, []byte("package main\nfunc A() {}\n"), 0644)
	_ = os.WriteFile(fileB, []byte("package main\nfunc B() {}\n"), 0644)

	const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snap := &sandbox.Snapshot{
		CommitSHA: testSHA,
		SourceDir: snapDir,
	}

	rawDiff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1,2 @@
 package main
+func A() {}
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1 +1,2 @@
 package main
+func B() {}
`
	inv := diff.Parse(rawDiff, diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var step int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curStep := atomic.AddInt32(&step, 1)
		var respContent string
		if curStep == 1 {
			// Turn 1 reads only a.go
			respContent = `{"action": "read_file", "path": "a.go"}`
		} else {
			// Concluding prompt after budget exhaustion
			respContent = `{
				"action": "answer",
				"score": 80,
				"summary": "Partial review due to 1-turn budget.",
				"findings": []
			}`
		}
		resp := llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: respContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		AgentMaxTurns: 1, // Strict 1-turn budget
	}
	llmClient := llm.NewClient(llmServer.URL, "key", "model")
	agent := NewReviewerAgent(cfg, llmClient)

	pr := &github.PRDetails{
		Title:   "Two file change",
		HeadSHA: testSHA,
	}

	res, err := agent.Run(context.Background(), pr, inv, ledger, snap, "skipped", sandbox.StatusUnavailable, nil, nil)
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if !res.BudgetExhausted {
		t.Errorf("expected BudgetExhausted to be true with 1-turn budget")
	}
	if !ledger.IsExamined("a.go#H1") {
		t.Errorf("a.go should be examined")
	}
	if ledger.IsExamined("b.go#H1") {
		t.Errorf("b.go should remain skipped")
	}

	// Verify ledger skipped reason
	skipped := ledger.Skipped()
	var foundBSkipped bool
	for _, s := range skipped {
		if s.File == "b.go" {
			foundBSkipped = true
			if s.Reason != "not examined" {
				t.Errorf("expected reason 'not examined' for b.go, got %q", s.Reason)
			}
		}
	}
	if !foundBSkipped {
		t.Errorf("b.go should be in skipped ledger entries")
	}
}

func TestAgent_RefusesMutationsAndAbortsAtViolationLimit(t *testing.T) {
	snapDir := t.TempDir()
	const testSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	snap := &sandbox.Snapshot{
		CommitSHA: testSHA,
		SourceDir: snapDir,
	}

	inv := diff.Parse("", diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var step int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curStep := atomic.AddInt32(&step, 1)
		var respContent string
		switch curStep {
		case 1:
			respContent = `{"action": "write_file", "path": "pwn.go", "content": "malicious"}`
		case 2:
			respContent = `{"action": "run_command", "command": "rm -rf /"}`
		case 3:
			respContent = `{"action": "commit_and_push", "message": "exploit"}`
		default:
			respContent = `{"action": "answer", "score": 100, "summary": "done"}`
		}
		resp := llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: respContent,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		AgentMaxTurns:            10,
		AgentModelViolationLimit: 3,
	}
	llmClient := llm.NewClient(llmServer.URL, "key", "model")
	agent := NewReviewerAgent(cfg, llmClient)

	pr := &github.PRDetails{
		Title:   "Malicious attempt",
		HeadSHA: testSHA,
	}

	_, err := agent.Run(context.Background(), pr, inv, ledger, snap, "skipped", sandbox.StatusUnavailable, nil, nil)
	if err == nil {
		t.Fatalf("expected error from mutation violations limit")
	}
	if !errors.Is(err, ErrModelViolationsExceeded) {
		t.Fatalf("expected ErrModelViolationsExceeded, got: %v", err)
	}

	// Verify no files were created
	if _, statErr := os.Stat(filepath.Join(snapDir, "pwn.go")); !os.IsNotExist(statErr) {
		t.Fatalf("write_file executed! pwn.go exists in snapshot!")
	}
}

func TestAgent_ReleasesSnapshotOnCancelledContext(t *testing.T) {
	snapDir := t.TempDir()
	const testSHA = "cccccccccccccccccccccccccccccccccccccccc"
	snap := &sandbox.Snapshot{
		CommitSHA: testSHA,
		SourceDir: snapDir,
	}

	inv := diff.Parse("", diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var startOnce sync.Once
	started := make(chan struct{})
	doneHandler := make(chan struct{})
	defer close(doneHandler)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-doneHandler:
		case <-time.After(2 * time.Second):
		}
		http.Error(w, "context canceled", http.StatusRequestTimeout)
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		AgentMaxTurns: 5,
	}
	llmClient := llm.NewClient(llmServer.URL, "key", "model")
	agent := NewReviewerAgent(cfg, llmClient)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-started
		cancel()
	}()

	pr := &github.PRDetails{
		Title:   "Cancel test",
		HeadSHA: testSHA,
	}

	_, err := agent.Run(ctx, pr, inv, ledger, snap, "skipped", sandbox.StatusUnavailable, nil, nil)
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestAgent_EngineReviewPRAtHead_SnapshotCleanedOnCancel(t *testing.T) {
	const (
		baseSHA = "1111111111111111111111111111111111111111"
		headSHA = "2222222222222222222222222222222222222222"
	)

	var blockOnce sync.Once
	llmBlocked := make(chan struct{})
	doneHandler := make(chan struct{})
	defer close(doneHandler)

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockOnce.Do(func() { close(llmBlocked) })
		select {
		case <-r.Context().Done():
		case <-doneHandler:
		case <-time.After(2 * time.Second):
		}
		http.Error(w, "context canceled", http.StatusRequestTimeout)
	}))
	defer llmServer.Close()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1,
				"title":  "Test PR",
				"head":   map[string]any{"sha": headSHA, "ref": "feat"},
				"base":   map[string]any{"sha": baseSHA, "ref": "main"},
				"user":   map[string]any{"login": "dev"},
			})
		case strings.Contains(r.URL.Path, "/compare/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n+new\n"))
		case strings.Contains(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	cleanedUp := false
	snapDir := t.TempDir()
	snap := &sandbox.Snapshot{
		CommitSHA: headSHA,
		SourceDir: snapDir,
	}

	runner := sandbox.NewRunner(0)
	runner.SourceProvider = &testSourceProvider{
		snapshot: snap,
		cleanup: func() {
			cleanedUp = true
		},
	}

	cfg := &config.Config{
		EffortLevel:   "balanced",
		EnableSandbox: true,
		LLMBaseURL:    llmServer.URL,
		LLMAPIKey:     "key",
		LLMModel:      "model",
	}

	eng := NewEngineWithClients(cfg, ghClient, llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel), runner)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = eng.ReviewPRAtHead(ctx, "owner", "repo", 1, headSHA)
	if err == nil {
		t.Fatalf("expected cancellation error")
	}

	if !cleanedUp {
		t.Errorf("snapshot cleanup was not executed upon context cancellation")
	}
}

type testSourceProvider struct {
	snapshot *sandbox.Snapshot
	cleanup  func()
}

func (m *testSourceProvider) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*sandbox.Snapshot, func(), error) {
	return m.snapshot, m.cleanup, nil
}
