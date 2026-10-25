package reviewer

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/retry"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

func TestAgent_HTTPTimeoutReturnsError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	client := llm.NewClient(srv.URL, "key", "model")
	client.SetTimeout(10 * time.Millisecond)
	client.SetRetryPolicy(retry.Policy{MaxAttempts: 1})
	a := NewReviewerAgent(&config.Config{AgentMaxTurns: 1}, client)
	result, err := a.Run(context.Background(), &github.PRDetails{Title: "timeout"}, nil, nil, nil, "", sandbox.StatusUnavailable, nil, nil)
	if err == nil || result != nil {
		t.Fatalf("provider timeout must return an error, not a nil successful result: result=%+v error=%v", result, err)
	}
}

func TestAgentPrompts_IndependentConcernCategories(t *testing.T) {
	a := &ReviewerAgent{}
	for _, status := range []sandbox.VerificationStatus{sandbox.StatusPassed, sandbox.StatusTestFailed, sandbox.StatusUnavailable} {
		t.Run(string(status), func(t *testing.T) {
			system := a.buildAgentSystemPrompt(status, 10)
			for _, want := range []string{
				buildSystemPrompt(status),
				"Finding one severe defect is not a reason to stop early or to cap the number of findings",
				"You have up to 10 iterative tool turns",
				"Do not guess unseen behavior or crawl unrelated files",
				`{"file":"path/to/file.go","line":42,"severity":"CRITICAL","title":"Concrete defect","description":"Evidence, triggering conditions, and impact","suggestion":"A safe correction"}`,
				"exact repository-relative file path and source line supported by the inspected code",
				"Severity is CRITICAL, WARNING, or NOTE",
				"Do not invent locations or unseen behavior",
				"Findings may be an empty array",
			} {
				if !strings.Contains(system, want) {
					t.Errorf("agent system prompt missing %q", want)
				}
			}
			initial := a.buildAgentInitialUserPrompt(&github.PRDetails{Title: "test"}, coverageInventory("a.go"), nil, nil, "", status)
			for _, want := range []string{
				"Your FIRST action must be action='read_file' on a modified file",
				"Read every modified non-test source file before you conclude",
				"weighing each against the applicable concern categories (security and input validation, concurrency, logic and correctness)",
				"then answer with action='answer'",
			} {
				if !strings.Contains(initial, want) {
					t.Errorf("agent initial prompt missing %q", want)
				}
			}
		})
	}
}

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

func TestConfinement_TraversalAndEscapes(t *testing.T) {
	snapDir := t.TempDir()
	secretFile := filepath.Join(snapDir, "safe.txt")
	_ = os.WriteFile(secretFile, []byte("safe content"), 0644)

	tools, err := NewSnapshotTools(snapDir, nil)
	if err != nil {
		t.Fatalf("failed to create tools: %v", err)
	}

	adversarialPaths := []struct {
		name         string
		path         string
		expectedRule string
	}{
		{"parent directory escape", "../outside.txt", "path outside workspace"},
		{"deep parent traversal", "../../etc/passwd", "path outside workspace"},
		{"unix root absolute", "/etc/passwd", "absolute paths are not permitted"},
		{"windows root absolute", "\\Windows\\system32", "absolute paths are not permitted"},
		{"windows drive absolute", "C:\\autoexec.bat", "absolute paths are not permitted"},
		{"windows drive relative", "C:config.sys", "absolute paths are not permitted"},
		{"windows drive forward slash", "d:/foo/bar", "absolute paths are not permitted"},
		{"null byte in path", "safe.txt\x00extra", "invalid path: contains null byte"},
		{"root git directory", ".git", ".git paths are restricted"},
		{"git config file", ".git/config", ".git paths are restricted"},
		{"nested git directory", "sub/.git/HEAD", ".git paths are restricted"},
		{"dot git with trailing dot", ".git.", ".git paths are restricted"},
	}

	for _, tc := range adversarialPaths {
		t.Run("ReadFile_"+tc.name, func(t *testing.T) {
			_, err := tools.ReadFile(tc.path)
			if err == nil {
				t.Fatalf("expected error reading %q, got nil", tc.path)
			}
			if !strings.Contains(err.Error(), tc.expectedRule) {
				t.Errorf("expected error containing %q, got: %v", tc.expectedRule, err)
			}
		})

		t.Run("ListFiles_"+tc.name, func(t *testing.T) {
			_, err := tools.ListFiles(tc.path)
			if err == nil {
				t.Fatalf("expected error listing %q, got nil", tc.path)
			}
			if !strings.Contains(err.Error(), tc.expectedRule) {
				t.Errorf("expected error containing %q, got: %v", tc.expectedRule, err)
			}
		})
	}
}

func TestConfinement_EscapingSymlink(t *testing.T) {
	baseDir := t.TempDir()
	outsideDir := filepath.Join(baseDir, "outside")
	insideDir := filepath.Join(baseDir, "snapshot")
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(insideDir, 0755); err != nil {
		t.Fatal(err)
	}

	outsideSecret := filepath.Join(outsideDir, "secret.key")
	_ = os.WriteFile(outsideSecret, []byte("super-secret-host-token"), 0600)

	symlinkPath := filepath.Join(insideDir, "symlink_escape")
	err := os.Symlink(outsideSecret, symlinkPath)
	if err != nil {
		// On Windows without Developer Mode, creating symlinks may require elevated privileges
		t.Skipf("skipping symlink test: symlink creation not permitted on this host: %v", err)
		return
	}

	const testSHA = "1234567890123456789012345678901234567890"
	snap := &sandbox.Snapshot{
		CommitSHA: testSHA,
		SourceDir: insideDir,
	}

	// 1. Snapshot.Validate must reject escaping symlink
	if vErr := snap.Validate(); vErr == nil {
		t.Errorf("Snapshot.Validate should have rejected escaping symlink")
	} else if !strings.Contains(vErr.Error(), "escaping symlink") {
		t.Errorf("expected error containing 'escaping symlink', got: %v", vErr)
	}

	// 2. SnapshotTools.ReadFile must also reject escaping symlink
	tools, err := NewSnapshotTools(insideDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rErr := tools.ReadFile("symlink_escape")
	if rErr == nil {
		t.Errorf("ReadFile should reject escaping symlink")
	} else if !strings.Contains(rErr.Error(), "access denied") && !strings.Contains(rErr.Error(), "outside") {
		t.Errorf("expected access denied for escaping symlink, got: %v", rErr)
	}
}

func TestBudget_SearchOversizedLogFile(t *testing.T) {
	snapDir := t.TempDir()

	// Create a 5MB text log file with needles sprinkled in
	logPath := filepath.Join(snapDir, "app.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Write 5MB in 64KB blocks, with 5 match lines total
	block := strings.Repeat("2026-10-08 12:00:00 INFO [worker] routine heartbeat event payload\n", 800)
	for i := 0; i < 80; i++ {
		if i == 5 || i == 15 || i == 25 {
			_, _ = f.WriteString("2026-10-08 12:00:01 ERROR [worker] CRITICAL_SECURITY_NEEDLE detected in auth token\n")
		}
		_, _ = f.WriteString(block)
	}
	_ = f.Close()

	cfg := &config.Config{
		AgentFileReadBytes:    30000,
		AgentSearchMaxMatches: 50,
		AgentMaxToolBytes:     204800,
	}

	tools, err := NewSnapshotTools(snapDir, cfg)
	if err != nil {
		t.Fatal(err)
	}

	out, err := tools.SearchFiles("CRITICAL_SECURITY_NEEDLE")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if !strings.Contains(out, "CRITICAL_SECURITY_NEEDLE") {
		t.Errorf("expected search output to contain matching needle, got: %s", out)
	}

	// Verify byte budget was debited by the search output size
	if tools.BytesConsumed() <= 0 {
		t.Errorf("expected search output to debit budget, got %d", tools.BytesConsumed())
	}
	if tools.BytesConsumed() != int64(len(out)) {
		t.Errorf("expected bytes consumed %d == output len %d", tools.BytesConsumed(), len(out))
	}
}

func TestBudget_OversizedSingleToolOutputTruncatesWithMarker(t *testing.T) {
	snapDir := t.TempDir()
	largeFile := filepath.Join(snapDir, "large.txt")
	// 50KB content
	content := strings.Repeat("A", 50000)
	if err := os.WriteFile(largeFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		AgentFileReadBytes: 10000, // 10KB per read cap
		AgentMaxToolBytes:  15000, // 15KB total tool budget
	}

	tools, err := NewSnapshotTools(snapDir, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. First read: capped at 10000 bytes with file truncation marker
	out1, err := tools.ReadFile("large.txt")
	if err != nil {
		t.Fatalf("read 1 failed: %v", err)
	}
	if !strings.Contains(out1, "...[file truncated, exceeded 10000 bytes]...") {
		t.Errorf("expected file truncation marker in output 1, got: %s", out1[len(out1)-100:])
	}
	if tools.BytesConsumed() <= 10000 {
		t.Errorf("expected bytes consumed > 10000, got %d", tools.BytesConsumed())
	}
	if tools.IsBudgetExhausted() {
		t.Errorf("budget should not be exhausted after first 10KB read under 15KB budget")
	}

	// 2. Second read: should hit total budget cap (15000) and truncate with tool budget marker
	out2, err := tools.ReadFile("large.txt")
	if err != nil {
		t.Fatalf("read 2 failed: %v", err)
	}
	if !strings.Contains(out2, "...[tool output truncated, budget exhausted]...") {
		t.Errorf("expected tool budget exhausted marker in output 2, got: %s", out2)
	}
	if !tools.IsBudgetExhausted() {
		t.Errorf("budget should be exhausted after second read")
	}

	// 3. Third read: output completely omitted
	out3, err := tools.ReadFile("large.txt")
	if err != nil {
		t.Fatalf("read 3 failed: %v", err)
	}
	if !strings.Contains(out3, "...[tool output omitted, budget exhausted]...") {
		t.Errorf("expected omitted marker in output 3, got: %s", out3)
	}
}

func TestHeadBinding_MismatchAbortsZeroLLMCalls(t *testing.T) {
	const (
		jobHeadSHA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		mismatchedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		baseSHA       = "cccccccccccccccccccccccccccccccccccccccc"
	)

	var llmCalls int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&llmCalls, 1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer llmServer.Close()

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42,
				"title":  "Head mismatch PR",
				"head":   map[string]any{"sha": jobHeadSHA, "ref": "feat"},
				"base":   map[string]any{"sha": baseSHA, "ref": "main"},
				"user":   map[string]any{"login": "dev"},
			})
		case strings.Contains(r.URL.Path, "/compare/"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("diff --git a/x.go b/x.go\n+new line\n"))
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

	// Snapshot provided with different commit SHA than job's expected head
	snapDir := t.TempDir()
	snap := &sandbox.Snapshot{
		CommitSHA: mismatchedSHA, // Mismatch with jobHeadSHA!
		SourceDir: snapDir,
	}

	runner := sandbox.NewRunner(0)
	runner.SourceProvider = &testSourceProvider{
		snapshot: snap,
		cleanup:  func() {},
	}

	cfg := &config.Config{
		EffortLevel:   "balanced",
		EnableSandbox: true,
		LLMBaseURL:    llmServer.URL,
		LLMAPIKey:     "key",
		LLMModel:      "model",
	}

	eng := NewEngineWithClients(cfg, ghClient, llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel), runner)

	// Attempt ReviewPRAtHead with jobHeadSHA
	report, err := eng.ReviewPRAtHead(context.Background(), "owner", "repo", 42, jobHeadSHA)
	if err == nil {
		t.Fatalf("expected head mismatch error, got report: %+v", report)
	}
	if !strings.Contains(err.Error(), "does not match PR head SHA") {
		t.Errorf("expected error containing mismatch message, got: %v", err)
	}

	// Assert ZERO completion requests were made
	if calls := atomic.LoadInt32(&llmCalls); calls != 0 {
		t.Fatalf("CRITICAL SECURITY VIOLATION: %d LLM completion calls were made despite head SHA mismatch!", calls)
	}
}

func TestConfinement_ConcurrentLoopsIsolated(t *testing.T) {
	snap1Dir := t.TempDir()
	snap2Dir := t.TempDir()

	_ = os.WriteFile(filepath.Join(snap1Dir, "secret1.txt"), []byte("SECRET-ALPHA-1"), 0600)
	_ = os.WriteFile(filepath.Join(snap2Dir, "secret2.txt"), []byte("SECRET-BETA-2"), 0600)

	tools1, err := NewSnapshotTools(snap1Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools2, err := NewSnapshotTools(snap2Dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCount := int32(0)

	for i := 0; i < 20; i++ {
		wg.Add(2)

		go func() {
			defer wg.Done()
			// Loop 1 can read its own secret
			content, err := tools1.ReadFile("secret1.txt")
			if err != nil || content != "SECRET-ALPHA-1" {
				atomic.AddInt32(&errCount, 1)
				return
			}
			// Loop 1 cannot read secret2.txt
			_, err = tools1.ReadFile("secret2.txt")
			if err == nil {
				atomic.AddInt32(&errCount, 1)
			}
		}()

		go func() {
			defer wg.Done()
			// Loop 2 can read its own secret
			content, err := tools2.ReadFile("secret2.txt")
			if err != nil || content != "SECRET-BETA-2" {
				atomic.AddInt32(&errCount, 1)
				return
			}
			// Loop 2 cannot read secret1.txt
			_, err = tools2.ReadFile("secret1.txt")
			if err == nil {
				atomic.AddInt32(&errCount, 1)
			}
		}()
	}

	wg.Wait()
	if count := atomic.LoadInt32(&errCount); count != 0 {
		t.Fatalf("concurrent loop isolation failed with %d errors", count)
	}
}

// REV-READ-01: an early "answer" before any modified file was served must be
// rejected once, and the model is pushed to read the changed file first.
func TestAgent_EarlyAnswerPushback_RequiresReadFile(t *testing.T) {
	snapDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapDir, "a.go"), []byte("package main\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const testSHA = "dddddddddddddddddddddddddddddddddddddddd"
	snap := &sandbox.Snapshot{CommitSHA: testSHA, SourceDir: snapDir}
	if err := snap.Validate(); err != nil {
		t.Fatalf("snapshot validate failed: %v", err)
	}

	rawDiff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package main
+func A() {}
`
	inv := diff.Parse(rawDiff, diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var step int32
	var mu sync.Mutex
	var userPrompts []string

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				userPrompts = append(userPrompts, msg.Content)
			}
		}
		mu.Unlock()

		curStep := atomic.AddInt32(&step, 1)
		var respContent string
		switch curStep {
		case 1:
			// Fabricated answer on turn 1, before reading anything.
			respContent = `{"action": "answer", "score": 42, "summary": "Fabricated review.", "findings": [{"file": "a.go", "line": 2, "severity": "CRITICAL", "title": "Invented bug", "description": "Never saw the code."}]}`
		case 2:
			respContent = `{"action": "read_file", "path": "a.go"}`
		default:
			respContent = `{"action": "answer", "score": 88, "summary": "Reviewed after reading a.go.", "findings": []}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{{
				Message:      llm.ChatMessage{Role: "assistant", Content: respContent},
				FinishReason: "stop",
			}},
		})
	}))
	defer llmServer.Close()

	cfg := &config.Config{AgentMaxTurns: 10}
	agent := NewReviewerAgent(cfg, llm.NewClient(llmServer.URL, "key", "model"))
	pr := &github.PRDetails{Title: "Early answer", HeadSHA: testSHA}

	res, err := agent.Run(context.Background(), pr, inv, ledger, snap, "skipped", sandbox.StatusUnavailable, nil, nil)
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if len(res.FilesServed) != 1 || res.FilesServed[0] != "a.go" {
		t.Fatalf("expected a.go to be served after pushback, got %v", res.FilesServed)
	}
	if !ledger.IsExamined("a.go#H1") {
		t.Errorf("a.go#H1 should be examined after the model read it")
	}
	if res.Output == nil || res.Output.Score != 88 {
		t.Fatalf("expected the post-read answer (score 88), got %+v", res.Output)
	}
	if res.TurnsUsed != 3 {
		t.Errorf("expected 3 turns (rejected answer, read, answer), got %d", res.TurnsUsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(userPrompts) < 2 {
		t.Fatalf("expected at least 2 user prompts, got %d", len(userPrompts))
	}
	if !strings.Contains(userPrompts[1], "Action rejected") {
		t.Errorf("expected pushback message in second prompt, got:\n%s", userPrompts[1])
	}
	if !strings.Contains(userPrompts[1], "action='read_file'") || !strings.Contains(userPrompts[1], "a.go") {
		t.Errorf("pushback must name read_file and the changed file, got:\n%s", userPrompts[1])
	}
}

// An insistent answer triggers one automatic read, then can finish with evidence.
func TestAgent_EarlyAnswerCompletion_Bounded(t *testing.T) {
	snapDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapDir, "a.go"), []byte("package main\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const testSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	snap := &sandbox.Snapshot{CommitSHA: testSHA, SourceDir: snapDir}

	rawDiff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package main
+func A() {}
`
	inv := diff.Parse(rawDiff, diff.DefaultParseOptions())
	ledger := diff.NewCoverageLedger(inv)

	var step int32
	var mu sync.Mutex
	pushbackCount := 0

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, msg := range req.Messages {
			if msg.Role == "user" && strings.Contains(msg.Content, "Action rejected") {
				pushbackCount++
				break
			}
		}
		mu.Unlock()
		atomic.AddInt32(&step, 1)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{{
				Message:      llm.ChatMessage{Role: "assistant", Content: `{"action": "answer", "score": 50, "summary": "Insisting.", "findings": []}`},
				FinishReason: "stop",
			}},
		})
	}))
	defer llmServer.Close()

	cfg := &config.Config{AgentMaxTurns: 5}
	agent := NewReviewerAgent(cfg, llm.NewClient(llmServer.URL, "key", "model"))
	pr := &github.PRDetails{Title: "Insistent answer", HeadSHA: testSHA}

	res, err := agent.Run(context.Background(), pr, inv, ledger, snap, "skipped", sandbox.StatusUnavailable, nil, nil)
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if res.TurnsUsed != 2 {
		t.Errorf("expected 2 turns (automatic read then acceptance), got %d", res.TurnsUsed)
	}
	if len(res.FilesServed) != 1 || res.FilesServed[0] != "a.go" || ledger.ExaminedCount() != 1 {
		t.Errorf("expected a.go delivered and covered, got %v", res.FilesServed)
	}
	if res.Output == nil {
		t.Fatalf("expected the second answer to be accepted")
	}
	if calls := atomic.LoadInt32(&step); calls != 2 {
		t.Errorf("expected exactly 2 LLM calls, got %d", calls)
	}

	mu.Lock()
	defer mu.Unlock()
	if pushbackCount != 1 {
		t.Errorf("expected one automatic completion, saw %d", pushbackCount)
	}
}

// Models commonly emit several concatenated action objects. Slicing from the
// first '{' to the last '}' made that invalid JSON, so the turn was silently
// discarded and no file was ever read (observed live on thozoz/aura-web PR #1).
func TestParseAgentAction_MultipleConcatenatedObjects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		path string
	}{
		{
			name: "two read_file objects",
			raw:  "{\"action\": \"read_file\", \"path\": \"a.go\"}\n{\"action\": \"read_file\", \"path\": \"b.go\"}",
			want: "read_file",
			path: "a.go",
		},
		{
			name: "four objects incl trailing prose",
			raw:  "Here you go:\n{\"action\": \"read_file\", \"path\": \"x.go\"}\n{\"action\": \"read_file\", \"path\": \"y.go\"}\nDone.",
			want: "read_file",
			path: "x.go",
		},
		{
			name: "single object unchanged",
			raw:  "{\"action\": \"list_files\", \"path\": \".\"}",
			want: "list_files",
			path: ".",
		},
		{
			name: "fenced code block",
			raw:  "```json\n{\"action\": \"answer\", \"score\": 90, \"summary\": \"ok\"}\n```",
			want: "answer",
			path: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentAction(tc.raw)
			if err != nil {
				t.Fatalf("parseAgentAction() error = %v", err)
			}
			if got.Action != tc.want {
				t.Errorf("Action = %q, want %q", got.Action, tc.want)
			}
			if got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
		})
	}
}

// Output with no JSON object at all must still fail loudly.
func TestParseAgentAction_NoObject(t *testing.T) {
	if _, err := parseAgentAction("I think this code is fine."); err == nil {
		t.Fatal("parseAgentAction() error = nil, want an error for JSON-free output")
	}
}

// A provider that returns an empty completion must not burn a review turn or
// fail the job outright: the same turn is re-requested, bounded by
// maxEmptyResponseRetries. Observed live (thozoz/aura-web PR #1) where a single
// empty response at turn 2 discarded an already-explored review.
func TestAgent_EmptyCompletionRetriesSameTurn(t *testing.T) {
	const filePath = "internal/site/search.go"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "site"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filePath), []byte("package site\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			// Provider returns no content at all.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`))
		case 2:
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`,
				`{"action": "read_file", "path": "`+filePath+`"}`)
		default:
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`,
				`{"action": "answer", "score": 90, "summary": "grounded review", "findings": []}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{AgentMaxTurns: 6}
	llmClient := llm.NewClient(srv.URL, "key", "model")
	llmClient.SetRetryPolicy(retry.Policy{MaxAttempts: 1, BaseDelay: time.Millisecond})

	ledger := diff.NewCoverageLedger(&diff.ChangeInventory{
		Files: []*diff.FileDiff{{Path: filePath, Hunks: []*diff.Hunk{{NewStart: 1, NewCount: 3}}}},
	})
	agent := NewReviewerAgent(cfg, llmClient)
	res, err := agent.Run(context.Background(), &github.PRDetails{
		Owner: "o", Repo: "r", Number: 1, HeadSHA: strings.Repeat("a", 40),
	}, &diff.ChangeInventory{Files: []*diff.FileDiff{{Path: filePath, Hunks: []*diff.Hunk{{NewStart: 1, NewCount: 3}}}}},
		ledger, &sandbox.Snapshot{SourceDir: dir, CommitSHA: strings.Repeat("a", 40)}, "", sandbox.StatusUnavailable, nil, nil)
	if err != nil {
		t.Fatalf("Run() error = %v, want the empty completion to be retried", err)
	}
	if res == nil || res.Output == nil {
		t.Fatal("Run() returned no review output")
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("provider calls = %d, want 3 (empty, read_file, answer)", calls)
	}
	if len(res.FilesServed) != 1 {
		t.Fatalf("FilesServed = %v, want the one examined file", res.FilesServed)
	}
}

// Models trained on a function-calling dialect emit XML tool-call markup instead
// of this protocol's JSON. Live thozoz/aura-web PR #2: the JSON decoder rejected
// every turn, no file was ever read, and the review was correctly refused as
// zero-coverage even though the model was behaving sensibly.
func TestParseAgentAction_XMLToolCalls(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		action  string
		path    string
		query   string
		summary string
	}{
		{
			name:   "read_file with path parameter",
			raw:    "<tool_calls>\n<invoke name=\"read_file\">\n<parameter name=\"path\">internal/site/search.go</parameter>\n</invoke>\n</tool_calls>",
			action: "read_file",
			path:   "internal/site/search.go",
		},
		{
			name:   "read with file_path alias",
			raw:    "<invoke name=\"read\"><parameter name=\"file_path\">cmd/auraweb/main.go</parameter></invoke>",
			action: "read_file",
			path:   "cmd/auraweb/main.go",
		},
		{
			name:   "list_files with dir alias",
			raw:    "<invoke name=\"ls\"><parameter name=\"dir\">internal/site</parameter></invoke>",
			action: "list_files",
			path:   "internal/site",
		},
		{
			name:   "search_files with pattern alias",
			raw:    "<invoke name=\"grep\"><parameter name=\"pattern\">searchCacheKey</parameter></invoke>",
			action: "search_files",
			query:  "searchCacheKey",
		},
		{
			name:    "answer with summary and score",
			raw:     "<invoke name=\"submit\"><parameter name=\"summary\">Looks correct.</parameter><parameter name=\"score\">88</parameter></invoke>",
			action:  "answer",
			summary: "Looks correct.",
		},
		{
			name:   "prose prefix before the tool block",
			raw:    "I'll read the modified file first.\n<tool_calls><invoke name=\"read_file\"><parameter name=\"path\">a.go</parameter></invoke></tool_calls>",
			action: "read_file",
			path:   "a.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentAction(tc.raw)
			if err != nil {
				t.Fatalf("parseAgentAction() error = %v", err)
			}
			if got.Action != tc.action {
				t.Errorf("Action = %q, want %q", got.Action, tc.action)
			}
			if tc.path != "" && got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
			if tc.query != "" && got.Query != tc.query {
				t.Errorf("Query = %q, want %q", got.Query, tc.query)
			}
			if tc.summary != "" && got.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.summary)
			}
			if tc.action == "answer" && (got.Score == nil || *got.Score != 88) {
				t.Errorf("Score = %v, want 88", got.Score)
			}
		})
	}
}

// A tool call with no usable argument is skipped in favour of a later well-formed
// one in the same reply, rather than spending the turn on a malformed call.
func TestParseAgentAction_XMLSkipsMalformedCall(t *testing.T) {
	raw := `<tool_calls>
<invoke name="read_file"></invoke>
<invoke name="read_file"><parameter name="path">good.go</parameter></invoke>
</tool_calls>`
	got, err := parseAgentAction(raw)
	if err != nil {
		t.Fatalf("parseAgentAction() error = %v", err)
	}
	if got.Path != "good.go" {
		t.Fatalf("Path = %q, want good.go (the malformed call must be skipped)", got.Path)
	}
}

// JSON must keep working unchanged now that XML is tried first.
func TestParseAgentAction_JSONStillParsed(t *testing.T) {
	got, err := parseAgentAction(`{"action": "read_file", "path": "x.go"}`)
	if err != nil {
		t.Fatalf("parseAgentAction() error = %v", err)
	}
	if got.Action != "read_file" || got.Path != "x.go" {
		t.Fatalf("got %+v, want read_file x.go", got)
	}
}

// Live thozoz/aura-web PR #3 spent 8 of 12 turns emitting DeepSeek DSML
// markup: the JSON decoder rejected it, so no file was ever read. The DSML
// prefix is optional, attributes may come in any order, and extra attributes
// such as string="true" must not hide the name.
func TestParseAgentAction_DeepSeekDSML(t *testing.T) {
	// Built rather than written literally: the DSML marker is a reserved token.
	p := "｜"
	dsml := func(name, attrs, body string) string {
		return "<" + p + "DSML" + p + " " + name + " " + attrs + ">" + body + "</" + p + "DSML" + p + " " + name + ">"
	}
	param := func(attrs, value string) string { return dsml("parameter", attrs, value) }
	invoke := func(attrs, body string) string { return dsml("invoke", attrs, body) }

	cases := []struct {
		name    string
		raw     string
		action  string
		path    string
		query   string
		summary string
		score   int
	}{
		{
			name:   "real log shape with fullwidth prefix and string attribute",
			raw:    invoke(`name="read_file"`, param(`name="path" string="true"`, "internal/site/search.go")),
			action: "read_file",
			path:   "internal/site/search.go",
		},
		{
			name:   "extra attribute before name, single quotes",
			raw:    invoke(`string="true" name="read"`, param(`string="true" name='file_path'`, "cmd/auraweb/main.go")),
			action: "read_file",
			path:   "cmd/auraweb/main.go",
		},
		{
			name:   "search with extra attribute after name",
			raw:    `<invoke name="search_files"><parameter name="query" string="true">searchCacheKey</parameter></invoke>`,
			action: "search_files",
			query:  "searchCacheKey",
		},
		{
			name:    "answer with score before summary and string attributes",
			raw:     invoke(`name="answer"`, param(`name="score"`, "88")+param(`name="summary" string="true"`, "Looks correct.")),
			action:  "answer",
			summary: "Looks correct.",
			score:   88,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAgentAction(tc.raw)
			if err != nil {
				t.Fatalf("parseAgentAction() error = %v", err)
			}
			if got.Action != tc.action {
				t.Errorf("Action = %q, want %q", got.Action, tc.action)
			}
			if tc.path != "" && got.Path != tc.path {
				t.Errorf("Path = %q, want %q", got.Path, tc.path)
			}
			if tc.query != "" && got.Query != tc.query {
				t.Errorf("Query = %q, want %q", got.Query, tc.query)
			}
			if tc.summary != "" && got.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.summary)
			}
			if tc.score != 0 && (got.Score == nil || *got.Score != tc.score) {
				t.Errorf("Score = %v, want %d", got.Score, tc.score)
			}
		})
	}
}

// A reply may legitimately batch several reads; they must all be extracted, in
// the order the model wrote them, across plain and prefixed tags.
func TestParseAgentActions_MultipleInOrder(t *testing.T) {
	p := "｜"
	dsmlRead := func(path string) string {
		return "<" + p + "DSML" + p + ` invoke name="read_file"><` + p + "DSML" + p + ` parameter name="path">` + path +
			"</" + p + "DSML" + p + " parameter></" + p + "DSML" + p + " invoke>"
	}
	raw := dsmlRead("a.go") +
		`<invoke name="read_file"><parameter name="path">b.go</parameter></invoke>` +
		dsmlRead("c.go")

	actions, err := parseAgentActions(raw)
	if err != nil {
		t.Fatalf("parseAgentActions() error = %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions))
	}
	for i, want := range []string{"a.go", "b.go", "c.go"} {
		if actions[i].Path != want {
			t.Errorf("actions[%d].Path = %q, want %q", i, actions[i].Path, want)
		}
	}
}

// Alternate wire formats must preserve the same finding evidence as JSON.
func TestParseAgentAction_XMLAnswerPreservesFindings(t *testing.T) {
	raw := `<invoke name="answer"><parameter name="summary">One independently supported defect.</parameter><parameter name="score">40</parameter><parameter name="findings">[{"file":"a.go","line":3,"severity":"high","title":"Unsafe input","description":"Untrusted input reaches the output."}]</parameter></invoke>`
	got, err := parseAgentAction(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 1 || got.Findings[0].File != "a.go" || got.Findings[0].Line != 3 || got.Findings[0].Title != "Unsafe input" {
		t.Fatalf("XML answer lost finding evidence: %+v", got)
	}
	prefix := "｜" + "DSML" + "｜ "
	dsml := strings.NewReplacer("<invoke", "<"+prefix+"invoke", "</invoke", "</"+prefix+"invoke", "<parameter", "<"+prefix+"parameter", "</parameter", "</"+prefix+"parameter").Replace(raw)
	if parsed, err := parseAgentAction(dsml); err != nil || len(parsed.Findings) != 1 || parsed.Findings[0].Title != got.Findings[0].Title {
		t.Fatalf("DSML must preserve finding evidence too: action=%+v error=%v", parsed, err)
	}
	bad := strings.Replace(raw, `[{"file":"a.go","line":3,"severity":"high","title":"Unsafe input","description":"Untrusted input reaches the output."}]`, `not-json`, 1)
	if _, err := parseAgentAction(bad); err == nil {
		t.Fatal("malformed XML findings must not silently become an empty review")
	}
	res, _, _ := runAgentReplies(t, map[string]string{"a.go": "package p\n"}, nil, []string{
		`{"action":"read_file","path":"a.go"}`, raw,
	})
	if res.TurnsUsed != 2 || len(res.Output.Findings) != 1 || res.Output.Findings[0].Description != got.Findings[0].Description {
		t.Fatalf("Run must preserve XML finding evidence without extra calls: %+v", res)
	}
}

// JSON batches must keep collecting every object, not just the first.
func TestParseAgentActions_JSONMultipleInOrder(t *testing.T) {
	actions, err := parseAgentActions(`{"action":"read_file","path":"a.go"}{"action":"read_file","path":"b.go"}`)
	if err != nil {
		t.Fatalf("parseAgentActions() error = %v", err)
	}
	if len(actions) != 2 || actions[0].Path != "a.go" || actions[1].Path != "b.go" {
		t.Fatalf("got %+v, want a.go then b.go", actions)
	}
}

// Ambiguous replies are rejected whole: half-parsing them would silently drop
// either the tool calls or the answer.
func TestParseAgentActions_RejectsAmbiguousReplies(t *testing.T) {
	xmlRead := `<invoke name="read_file"><parameter name="path">a.go</parameter></invoke>`
	cases := []struct {
		name string
		raw  string
	}{
		{"json and xml", xmlRead + `{"action":"read_file","path":"b.go"}`},
		{"answer with tool call", xmlRead + `<invoke name="answer"><parameter name="summary">done</parameter></invoke>`},
		{"two answers", `<invoke name="answer"><parameter name="summary">one</parameter></invoke><invoke name="answer"><parameter name="summary">two</parameter></invoke>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAgentActions(tc.raw); !errors.Is(err, ErrAmbiguousAgentReply) {
				t.Fatalf("error = %v, want ErrAmbiguousAgentReply", err)
			}
		})
	}
}

// Broken or unknown markup must not execute anything; an unknown call is
// skipped so a later valid call in the same reply still runs.
func TestParseAgentActions_MalformedAndUnknown(t *testing.T) {
	bad := []string{
		`<invoke name="read_file"><parameter name="path">a.go`,
		`<invoke name="delete_repository"><parameter name="path">a.go</parameter></invoke>`,
		`&lt;invoke name="read_file"&gt;`,
	}
	for _, raw := range bad {
		if actions, err := parseAgentActions(raw); err == nil {
			t.Errorf("parseAgentActions(%q) = %+v, want an error", raw, actions)
		}
	}

	actions, err := parseAgentActions(`<invoke name="delete_repository"><parameter name="path">x</parameter></invoke><invoke name="read_file"><parameter name="path">good.go</parameter></invoke>`)
	if err != nil {
		t.Fatalf("parseAgentActions() error = %v", err)
	}
	if len(actions) != 1 || actions[0].Path != "good.go" {
		t.Fatalf("got %+v, want only the valid read of good.go", actions)
	}
}

func coverageInventory(paths ...string) *diff.ChangeInventory {
	inv := &diff.ChangeInventory{}
	for _, path := range paths {
		inv.Files = append(inv.Files, &diff.FileDiff{Path: path, Hunks: []*diff.Hunk{{NewStart: 1, NewCount: 1}}})
	}
	return inv
}

func TestAgent_CoverageCompletionAcrossBatches(t *testing.T) {
	paths := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go"}
	files := make(map[string]string)
	for _, path := range paths {
		files[path] = "package p\n// evidence for " + path + "\n"
	}
	res, ledger, prompts := runAgentReplies(t, files, nil, []string{
		`{"action":"read_file","path":"a.go"}`,
		`{"action":"answer","score":90,"summary":"done","findings":[]}`,
	}, coverageInventory(paths...))
	if res.TurnsUsed != 4 || ledger.ExaminedCount() != len(paths) {
		t.Fatalf("turns=%d examined=%d; want 4 turns and 8 hunks", res.TurnsUsed, ledger.ExaminedCount())
	}
	if strings.Join(res.FilesServed, ",") != strings.Join(paths, ",") {
		t.Fatalf("read order = %v, want %v", res.FilesServed, paths)
	}
	if len(prompts) != 4 || !strings.Contains(prompts[3], "evidence for h.go") {
		t.Fatal("final model call must see the last automatically supplied file")
	}
	for _, want := range []string{
		"Action rejected: changed files remain unread",
		"investigate relevant definitions, callers and tests before answering again",
		"weigh each newly served file against the other applicable categories (security and input validation, concurrency, logic)",
		"a severe defect in one category does not end the review",
	} {
		if !strings.Contains(prompts[3], want) {
			t.Errorf("coverage completion prompt missing %q", want)
		}
	}
	if len(res.Output.Findings) != 0 {
		t.Fatal("independent concern inspection must still accept an empty findings list")
	}
}

func TestAgent_CoverageCompletionReferenceDoesNotClearChangedFile(t *testing.T) {
	res, ledger, prompts := runAgentReplies(t, map[string]string{
		"reference.go": "package p\n", "a.go": "package p\n// changed evidence\n",
	}, nil, []string{
		`{"action":"read_file","path":"reference.go"}`,
		`{"action":"answer","summary":"done"}`,
	}, coverageInventory("a.go"))
	if res.TurnsUsed != 3 || ledger.ExaminedCount() != 1 || len(res.FilesServed) != 2 {
		t.Fatalf("unexpected result: turns=%d files=%v coverage=%d", res.TurnsUsed, res.FilesServed, ledger.ExaminedCount())
	}
	if !strings.Contains(prompts[2], "changed evidence") {
		t.Fatal("answer must be reconsidered after reading the changed file")
	}
}

func TestAgent_CoverageCompletionFailedAndTruncatedNotRetried(t *testing.T) {
	res, ledger, prompts := runAgentReplies(t, map[string]string{
		"a.go": "a", "big.go": strings.Repeat("b", 100),
	}, &config.Config{AgentFileReadBytes: 8}, []string{
		`{"action":"read_file","path":"a.go"}`,
		`{"action":"answer","summary":"partial"}`,
	}, coverageInventory("a.go", "big.go", "missing.go"))
	if res.TurnsUsed != 3 || ledger.ExaminedCount() != 1 {
		t.Fatalf("turns=%d coverage=%d; want one completion and only a.go covered", res.TurnsUsed, ledger.ExaminedCount())
	}
	if strings.Count(prompts[len(prompts)-1], "path: missing.go") != 1 || strings.Count(prompts[len(prompts)-1], "path: big.go") != 1 {
		t.Fatal("failed and truncated paths must only be attempted once automatically")
	}
}

func TestAgent_CoverageCompletionRespectsLastTurn(t *testing.T) {
	res, ledger, _ := runAgentReplies(t, map[string]string{"a.go": "a", "b.go": "b"},
		&config.Config{AgentMaxTurns: 2}, []string{
			`{"action":"read_file","path":"a.go"}`,
			`{"action":"answer","summary":"partial"}`,
		}, coverageInventory("a.go", "b.go"))
	if res.TurnsUsed != 2 || ledger.ExaminedCount() != 1 || len(res.FilesServed) != 1 {
		t.Fatalf("must not deliver unseen evidence on the last turn: %+v coverage=%d", res, ledger.ExaminedCount())
	}
}

func TestAgent_CoverageCompletionRespectsByteBudget(t *testing.T) {
	res, ledger, _ := runAgentReplies(t, map[string]string{"a.go": "abcd", "b.go": "efgh", "c.go": "ijkl"},
		&config.Config{AgentMaxToolBytes: 5}, []string{
			`{"action":"read_file","path":"a.go"}`,
			`{"action":"answer","summary":"partial"}`,
		}, coverageInventory("a.go", "b.go", "c.go"))
	if !res.BudgetExhausted || ledger.ExaminedCount() != 1 || len(res.FilesServed) != 2 {
		t.Fatalf("must stop during completion without covering truncated b.go: %+v coverage=%d", res, ledger.ExaminedCount())
	}
}

func TestRemainingReadActionsSkipsUnsupportedAndAttempted(t *testing.T) {
	inv := coverageInventory("a.go", "deleted.go", "binary.go", "mode.go", "empty.go", "b.go", "b.go")
	inv.Files[1].IsDeleted = true
	inv.Files[2].IsBinary = true
	inv.Files[3].IsModeOnly = true
	inv.Files[4].Hunks = nil
	actions := remainingReadActions(inv, map[string]bool{"a.go": true})
	if len(actions) != 1 || actions[0].Path != "b.go" {
		t.Fatalf("unexpected required actions: %+v", actions)
	}
}

func TestReadFileCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   string
		readLimit int64
		toolLimit int64
		complete  bool
	}{
		{"exact read cap", "abcdefgh", 8, 100, true},
		{"exact total budget", "abcdefgh", 8, 8, true},
		{"file cap", "abcdefghi", 8, 100, false},
		{"total budget", "abcdefgh", 8, 7, false},
		{"literal truncation marker", "...[tool output omitted, budget exhausted]...", 100, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			tools, err := NewSnapshotTools(dir, &config.Config{AgentFileReadBytes: tc.readLimit, AgentMaxToolBytes: tc.toolLimit})
			if err != nil {
				t.Fatal(err)
			}
			_, complete, err := tools.readFile("a.go")
			if err != nil || complete != tc.complete {
				t.Fatalf("complete=%v err=%v, want %v", complete, err, tc.complete)
			}
		})
	}
}

// dsmlRead renders one prefixed read_file call. The marker is concatenated so
// this source never contains it literally.
func dsmlRead(path string) string {
	p := "｜"
	return "<" + p + "DSML" + p + ` invoke name="read_file"><` + p + "DSML" + p + ` parameter name="path" string="true">` +
		path + "</" + p + "DSML" + p + " parameter></" + p + "DSML" + p + " invoke>"
}

// runAgentReplies drives a ReviewerAgent against a scripted LLM and returns the
// result, ledger, and the user prompts seen on each call.
func runAgentReplies(t *testing.T, files map[string]string, cfg *config.Config, replies []string, inventories ...*diff.ChangeInventory) (*AgentReviewResult, *diff.CoverageLedger, []string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	var step int32
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" {
				prompts = append(prompts, m.Content)
			}
		}
		mu.Unlock()

		n := int(atomic.AddInt32(&step, 1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{Choices: []llm.ChatChoice{{
			Message:      llm.ChatMessage{Role: "assistant", Content: replies[n]},
			FinishReason: "stop",
		}}})
	}))
	defer srv.Close()

	if cfg == nil {
		cfg = &config.Config{}
	}
	if cfg.AgentMaxTurns == 0 {
		cfg.AgentMaxTurns = 10
	}
	const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	inv := &diff.ChangeInventory{Files: []*diff.FileDiff{{Path: "a.go", Hunks: []*diff.Hunk{{NewStart: 1, NewCount: 2}}}}}
	if len(inventories) > 0 {
		inv = inventories[0]
	}
	ledger := diff.NewCoverageLedger(inv)
	agent := NewReviewerAgent(cfg, llm.NewClient(srv.URL, "key", "model"))
	res, err := agent.Run(context.Background(), &github.PRDetails{Title: "t", HeadSHA: testSHA}, inv, ledger,
		&sandbox.Snapshot{SourceDir: dir, CommitSHA: testSHA}, "", sandbox.StatusUnavailable, nil, nil)
	if err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}
	return res, ledger, prompts
}

// One DSML reply batching three reads must execute all three in one turn, in
// the order written, instead of spending (and losing) three turns.
func TestAgent_DSMLBatchExecutesInOrder(t *testing.T) {
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n", "c.go": "package c\n"}
	res, ledger, _ := runAgentReplies(t, files, nil, []string{
		dsmlRead("a.go") + dsmlRead("b.go") + dsmlRead("c.go"),
		`{"action": "answer", "score": 90, "summary": "read all three", "findings": []}`,
	})
	if res.TurnsUsed != 2 {
		t.Errorf("TurnsUsed = %d, want 2 (one batch turn plus the answer)", res.TurnsUsed)
	}
	want := []string{"a.go", "b.go", "c.go"}
	if len(res.FilesServed) != len(want) {
		t.Fatalf("FilesServed = %v, want %v", res.FilesServed, want)
	}
	for i := range want {
		if res.FilesServed[i] != want[i] {
			t.Errorf("FilesServed[%d] = %q, want %q", i, res.FilesServed[i], want[i])
		}
	}
	if ledger.ExaminedCount() == 0 {
		t.Errorf("batch read must still mark coverage")
	}
}

// The per-reply cap bounds work; the actions beyond it are reported, not
// silently discarded.
func TestAgent_DSMLBatchCappedAtFive(t *testing.T) {
	files := map[string]string{}
	var batch string
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		files[name] = "package x\n"
		batch += dsmlRead(name)
	}
	res, _, prompts := runAgentReplies(t, files, nil, []string{
		batch,
		`{"action": "answer", "score": 90, "summary": "done", "findings": []}`,
	})
	if len(res.FilesServed) != maxActionsPerReply {
		t.Fatalf("FilesServed = %v, want %d", res.FilesServed, maxActionsPerReply)
	}
	for _, served := range res.FilesServed {
		if served == "f.go" {
			t.Errorf("sixth read must not run in the same reply, got %v", res.FilesServed)
		}
	}
	if len(prompts) < 2 || !strings.Contains(prompts[1], "not executed") {
		t.Errorf("reply must report the skipped action, got prompts %v", prompts)
	}
}

// A reply that runs out of tool byte budget mid-batch must stop there, keep
// what it read, and not error.
func TestAgent_DSMLMidBatchBudgetExhaustion(t *testing.T) {
	files := map[string]string{"a.go": "package main\n\nfunc A() {}\n", "b.go": "package main\n\nfunc A() {}\n", "c.go": "package main\n\nfunc A() {}\n"}
	cfg := &config.Config{AgentMaxToolBytes: 40}
	res, _, _ := runAgentReplies(t, files, cfg, []string{
		dsmlRead("a.go") + dsmlRead("b.go") + dsmlRead("c.go"),
		`{"action": "answer", "score": 80, "summary": "partial", "findings": []}`,
	})
	if !res.BudgetExhausted {
		t.Errorf("BudgetExhausted = false, want true")
	}
	if len(res.FilesServed) != 2 {
		t.Errorf("FilesServed = %v, want 2 (third read must not run after exhaustion)", res.FilesServed)
	}
	if res.TurnsUsed > 3 {
		t.Errorf("TurnsUsed = %d, mid-batch exhaustion must not spin", res.TurnsUsed)
	}
}

// An answer mixed with tool calls is rejected whole and nudged; the tool call
// in it must not execute.
func TestAgent_MixedAnswerAndToolNudged(t *testing.T) {
	files := map[string]string{"a.go": "package a\n"}
	res, ledger, prompts := runAgentReplies(t, files, nil, []string{
		dsmlRead("a.go") + `<invoke name="answer"><parameter name="summary">premature</parameter></invoke>`,
		dsmlRead("a.go"),
		`{"action": "answer", "score": 90, "summary": "after reading", "findings": []}`,
	})
	if len(res.FilesServed) != 1 || res.FilesServed[0] != "a.go" {
		t.Fatalf("FilesServed = %v, want a.go served once by the clean reply", res.FilesServed)
	}
	if res.TurnsUsed != 3 {
		t.Errorf("TurnsUsed = %d, want 3 (rejected, read, answer)", res.TurnsUsed)
	}
	if ledger.ExaminedCount() == 0 {
		t.Errorf("clean reply must still mark coverage")
	}
	if len(prompts) < 2 || !strings.Contains(prompts[1], "Action rejected") {
		t.Errorf("mixed reply must be nudged, prompts = %v", prompts)
	}
}
