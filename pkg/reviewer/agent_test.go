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

func TestConfinement_TraversalAndEscapes(t *testing.T) {
	snapDir := t.TempDir()
	secretFile := filepath.Join(snapDir, "safe.txt")
	_ = os.WriteFile(secretFile, []byte("safe content"), 0644)

	tools, err := NewSnapshotTools(snapDir, nil)
	if err != nil {
		t.Fatalf("failed to create tools: %v", err)
	}

	adversarialPaths := []struct {
		name          string
		path          string
		expectedRule  string
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
		AgentFileReadBytes: 10000,  // 10KB per read cap
		AgentMaxToolBytes:  15000,  // 15KB total tool budget
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
		jobHeadSHA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		mismatchedSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		baseSHA         = "cccccccccccccccccccccccccccccccccccccccc"
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
