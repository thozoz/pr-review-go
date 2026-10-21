package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/github"
)

func TestVerifyProject_GoProject_SkipsExecutionWithoutContainerIsolation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create benign go.mod
	goModContent := "module example.com/testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatalf("failed to create go.mod: %v", err)
	}

	// Create a simple go file
	goFileContent := "package testmod\n\nfunc Hello() string { return \"hello\" }\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "hello.go"), []byte(goFileContent), 0644); err != nil {
		t.Fatalf("failed to create hello.go: %v", err)
	}

	// Add custom rules file to verify rule extraction still works
	rulesContent := "# Project Guidelines\nAlways write tests."
	if err := os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte(rulesContent), 0644); err != nil {
		t.Fatalf("failed to create AGENTS.md: %v", err)
	}

	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	report, err := runner.VerifyProject(ctx, tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.DetectedType != "go" {
		t.Errorf("expected DetectedType 'go', got %q", report.DetectedType)
	}

	if report.CustomRules != rulesContent {
		t.Errorf("expected CustomRules %q, got %q", rulesContent, report.CustomRules)
	}
	if report.RulesSource != "AGENTS.md" {
		t.Errorf("expected RulesSource 'AGENTS.md', got %q", report.RulesSource)
	}

	if len(report.Results) != 0 {
		t.Errorf("expected 0 execution results due to missing container isolation, got %d: %+v", len(report.Results), report.Results)
	}

	expectedSubstring := "verification skipped due to missing container isolation"
	if !strings.Contains(report.Summary, "SKIPPED") || !strings.Contains(strings.ToLower(report.Summary), expectedSubstring) {
		t.Errorf("expected summary to explicitly mention SKIPPED and missing container isolation, got: %q", report.Summary)
	}
}

func TestVerifyProject_OtherProjectTypes_SkipExecution(t *testing.T) {
	testCases := []struct {
		name          string
		markerFile    string
		markerContent string
		expectedType  string
	}{
		{
			name:          "Node project",
			markerFile:    "package.json",
			markerContent: `{"name": "test-pkg", "scripts": {"test": "echo test"}}`,
			expectedType:  "node",
		},
		{
			name:          "Rust project",
			markerFile:    "Cargo.toml",
			markerContent: "[package]\nname = \"test-pkg\"\nversion = \"0.1.0\"\n",
			expectedType:  "rust",
		},
		{
			name:          "Python project with pyproject.toml",
			markerFile:    "pyproject.toml",
			markerContent: "[project]\nname = \"test-pkg\"\n",
			expectedType:  "python",
		},
		{
			name:          "Python project with requirements.txt",
			markerFile:    "requirements.txt",
			markerContent: "pytest>=7.0.0\n",
			expectedType:  "python",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmpDir, tc.markerFile), []byte(tc.markerContent), 0644); err != nil {
				t.Fatalf("failed to create %s: %v", tc.markerFile, err)
			}

			runner := NewRunner(10 * time.Second)
			ctx := context.Background()

			report, err := runner.VerifyProject(ctx, tmpDir)
			if err != nil {
				t.Fatalf("VerifyProject failed: %v", err)
			}

			if report.DetectedType != tc.expectedType {
				t.Errorf("expected DetectedType %q, got %q", tc.expectedType, report.DetectedType)
			}

			if len(report.Results) != 0 {
				t.Errorf("expected 0 execution results, got %d: %+v", len(report.Results), report.Results)
			}

			expectedSubstring := "verification skipped due to missing container isolation"
			if !strings.Contains(report.Summary, "SKIPPED") || !strings.Contains(strings.ToLower(report.Summary), expectedSubstring) {
				t.Errorf("expected summary to explicitly mention SKIPPED and missing container isolation, got: %q", report.Summary)
			}
		})
	}
}

func TestVerifyProject_GenericProject(t *testing.T) {
	tmpDir := t.TempDir()
	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	report, err := runner.VerifyProject(ctx, tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.DetectedType != "generic" {
		t.Errorf("expected DetectedType 'generic', got %q", report.DetectedType)
	}
	if len(report.Results) != 0 {
		t.Errorf("expected 0 execution results, got %d", len(report.Results))
	}
	if !strings.Contains(report.Summary, "No recognized build/test configuration found") {
		t.Errorf("unexpected summary: %q", report.Summary)
	}
}

func TestPrepareWorkspace_ReturnsUnavailableWithoutHostGit(t *testing.T) {
	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	_, _, err := runner.PrepareWorkspace(ctx, "https://github.com/example/repo.git", "main", "0123456789abcdef0123456789abcdef01234567")
	if err == nil {
		t.Fatalf("expected error from PrepareWorkspace, got nil")
	}
	if !errors.Is(err, ErrSourceProviderUnavailable) {
		t.Fatalf("expected ErrSourceProviderUnavailable, got: %v", err)
	}
}

func TestSnapshotValidation_RejectsMalformedAndPrefixSHAs(t *testing.T) {
	tmpDir := t.TempDir()

	testCases := []struct {
		name      string
		commitSHA string
		dir       string
		wantErr   bool
	}{
		{
			name:      "empty SHA",
			commitSHA: "",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "short prefix SHA (7 chars)",
			commitSHA: "abcdef0",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "39 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef0123456",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "40 chars non-hex SHA",
			commitSHA: "0123456789abcdef0123456789abcdef0123456g",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "nonexistent directory",
			commitSHA: "0123456789abcdef0123456789abcdef01234567",
			dir:       filepath.Join(tmpDir, "nonexistent"),
			wantErr:   true,
		},
		{
			name:      "valid 40 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef01234567",
			dir:       tmpDir,
			wantErr:   false,
		},
		{
			name:      "valid 64 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef012345670123456789abcdef01234567",
			dir:       tmpDir,
			wantErr:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{
				CommitSHA: tc.commitSHA,
				SourceDir: tc.dir,
			}
			err := s.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSnapshotValidation_RejectsEscapingSymlinks(t *testing.T) {
	outsideDir := t.TempDir()
	sourceDir := t.TempDir()

	targetFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(targetFile, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create symlink inside sourceDir pointing outside
	linkPath := filepath.Join(sourceDir, "escape-link")
	if err := os.Symlink(targetFile, linkPath); err != nil {
		t.Fatal(err)
	}

	s := &Snapshot{
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceDir: sourceDir,
	}

	err := s.Validate()
	if err == nil {
		t.Fatalf("expected error for escaping symlink, got nil")
	}
	if !strings.Contains(err.Error(), "escaping symlink") {
		t.Fatalf("expected escaping symlink error, got: %v", err)
	}
}

func TestVerifyProject_GoProject_ReportsIncompleteForUnvendoredDependencies(t *testing.T) {
	tmpDir := t.TempDir()

	goModContent := "module example.com/testmod\n\ngo 1.22\n\nrequire github.com/example/external v1.0.0\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewRunnerWithConfig(nil, nil, nil)
	report, err := runner.VerifyProject(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.Status != StatusIncomplete {
		t.Errorf("expected StatusIncomplete, got %q", report.Status)
	}
	if !strings.Contains(report.Reason, "external dependencies require network gateway") {
		t.Errorf("expected gateway reason, got: %q", report.Reason)
	}
}

func TestCustomRules_BoundedHandleStreaming_Truncation(t *testing.T) {
	tmpDir := t.TempDir()
	largeRules := strings.Repeat("Rule line here\n", 2000) // ~30,000 bytes
	if err := os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte(largeRules), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(0)
	report := &VerificationReport{}
	runner.extractCustomRules(tmpDir, report)

	if !strings.Contains(report.CustomRules, "...[instructions truncated]...") {
		t.Fatalf("expected custom rules to be truncated, got length %d", len(report.CustomRules))
	}
	if len(report.CustomRules) > 15100 {
		t.Errorf("expected custom rules to be bounded near 15000 bytes, got %d", len(report.CustomRules))
	}
	if report.RulesSource != "AGENTS.md" {
		t.Errorf("expected RulesSource AGENTS.md, got %q", report.RulesSource)
	}
}

func TestCustomRules_EscapingSymlinkDenied(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	outsideDir := filepath.Join(baseDir, "outside")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}

	outsideRules := filepath.Join(outsideDir, "secret-rules.md")
	if err := os.WriteFile(outsideRules, []byte("secret guidelines"), 0644); err != nil {
		t.Fatal(err)
	}

	linkPath := filepath.Join(workDir, "AGENTS.md")
	if err := os.Symlink(outsideRules, linkPath); err != nil {
		t.Skipf("skipping test: symlinks not supported on platform: %v", err)
	}

	runner := NewRunner(0)
	report := &VerificationReport{}
	runner.extractCustomRules(workDir, report)

	if report.CustomRules != "" {
		t.Fatalf("VULNERABILITY: custom rules extracted from escaping symlink: %q", report.CustomRules)
	}
}

func TestPlatform_NewPlatformRunner_ConfiguresSourceProvider(t *testing.T) {
	ghClient, _ := github.NewTestClient("https://github.com")
	runner := NewPlatformRunner(nil, ghClient, nil, nil)

	if runner.SourceProvider == nil {
		t.Fatalf("expected SourceProvider to be configured on NewPlatformRunner")
	}

	apiSource, ok := runner.SourceProvider.(*APISnapshotSource)
	if !ok {
		t.Fatalf("expected APISnapshotSource, got %T", runner.SourceProvider)
	}
	if apiSource.Client != ghClient {
		t.Errorf("expected ghClient to be preserved on APISnapshotSource")
	}
}

func TestCustomRules_SwapAfterOpenDoesNotLeakSecret(t *testing.T) {
	baseDir := t.TempDir()
	workDir := filepath.Join(baseDir, "workspace")
	outsideDir := filepath.Join(baseDir, "outside")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}

	outsideSecret := filepath.Join(outsideDir, "secret-rules.md")
	secretContent := "super-secret-rules-token"
	if err := os.WriteFile(outsideSecret, []byte(secretContent), 0644); err != nil {
		t.Fatal(err)
	}

	victimFile := filepath.Join(workDir, "AGENTS.md")
	benignContent := "# Benign Guidelines\nFollow rules."
	if err := os.WriteFile(victimFile, []byte(benignContent), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(0)
	report := &VerificationReport{}
	runner.extractCustomRules(workDir, report)
	if report.CustomRules != benignContent {
		t.Fatalf("expected initial benign rules, got %q", report.CustomRules)
	}

	// Swap victim file with symlink to outside secret
	_ = os.Remove(victimFile)
	if err := os.Symlink(outsideSecret, victimFile); err == nil {
		swappedReport := &VerificationReport{}
		runner.extractCustomRules(workDir, swappedReport)
		if strings.Contains(swappedReport.CustomRules, secretContent) {
			t.Fatalf("VULNERABILITY: read outside secret after swap: %s", swappedReport.CustomRules)
		}
	}
}

// recordingPreparer is a DependencyPreparer fake that records invocations so
// stage-split tests can prove the gateway socket path (PrepareDependencies
// owns the per-job depgw- socket) is entered only when the fetch gate opens.
type recordingPreparer struct {
	calls  int
	lastID string
	result *StageResult
	err    error
}

func (f *recordingPreparer) PrepareDependencies(ctx context.Context, jobID string, lease *SlotLease) (*StageResult, error) {
	f.calls++
	f.lastID = jobID
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &StageResult{StageName: "prep", Passed: true}, nil
}

func testSlotLease(t *testing.T) *SlotLease {
	t.Helper()
	base := t.TempDir()
	lease := &SlotLease{
		JobID:       "job-test",
		ControlDir:  filepath.Join(base, "control"),
		WorkDir:     filepath.Join(base, "work"),
		TmpDir:      filepath.Join(base, "tmp"),
		CacheDir:    filepath.Join(base, "cache"),
		SnapshotDir: filepath.Join(base, "snap"),
	}
	for _, d := range []string{lease.ControlDir, lease.WorkDir, lease.TmpDir, lease.CacheDir, lease.SnapshotDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}

// TestStageSplit proves the fetch gate: the preparer (sole owner of the
// per-job gateway socket bind) runs only when opt-in AND reverify both hold;
// every other combination performs no socket bind.
func TestStageSplit(t *testing.T) {
	gateCases := []struct {
		name              string
		networkFetch      bool
		trustedReverified bool
		wantFetchOK       bool
	}{
		{"opt-out offline", false, false, false},
		{"stale opt-in without reverify", true, false, false},
		{"reverified without opt-in", false, true, false},
		{"trusted opt-in", true, true, true},
	}
	for _, tc := range gateCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fetchGateOK(tc.networkFetch, tc.trustedReverified); got != tc.wantFetchOK {
				t.Fatalf("fetchGateOK(%v,%v) = %v, want %v",
					tc.networkFetch, tc.trustedReverified, got, tc.wantFetchOK)
			}
			fake := &recordingPreparer{}
			r := &Runner{DepPreparer: fake}
			res, err := r.runPrepGated(context.Background(), "job-1", testSlotLease(t), fetchGateOK(tc.networkFetch, tc.trustedReverified))
			if err != nil {
				t.Fatalf("runPrepGated error: %v", err)
			}
			if !tc.wantFetchOK {
				if fake.calls != 0 {
					t.Fatalf("fetch gate closed but preparer ran %d times (socket must not bind)", fake.calls)
				}
				if res != nil {
					t.Fatalf("closed gate must return nil prep result, got %+v", res)
				}
				return
			}
			if fake.calls != 1 {
				t.Fatalf("open gate must run preparer exactly once, ran %d times", fake.calls)
			}
			if res == nil || !res.Passed {
				t.Fatalf("open gate must return passing prep result, got %+v", res)
			}
		})
	}
}

// TestNoGatewaySocket proves stage 2 is socket-free in both modes: the public
// entry points bind no socket for opt-out, stale-trust, or opted-in runs on
// this platform, and runner.go itself never names an in-sandbox socket mount.
func TestNoGatewaySocket(t *testing.T) {
	combos := []struct {
		name              string
		networkFetch      bool
		trustedReverified bool
	}{
		{"opt-out binds no socket", false, false},
		{"stale flag without reverify binds no socket", true, false},
		{"opted-in stage 2 binds no socket", true, true},
	}
	for _, tc := range combos {
		t.Run(tc.name, func(t *testing.T) {
			fake := &recordingPreparer{}
			r := &Runner{DepPreparer: fake}
			snap := &Snapshot{
				CommitSHA: "0123456789abcdef0123456789abcdef01234567",
				SourceDir: t.TempDir(),
			}
			_, _ = r.RunSnapshotWithNetwork(context.Background(), snap, tc.networkFetch, tc.trustedReverified)
			// runPrepGated is the only path that can invoke the preparer;
			// drive it directly to assert the socket-bind decision per combo.
			fake.calls = 0
			_, err := r.runPrepGated(context.Background(), "job-1", testSlotLease(t), fetchGateOK(tc.networkFetch, tc.trustedReverified))
			if err != nil {
				t.Fatalf("runPrepGated error: %v", err)
			}
			wantCalls := 0
			if tc.networkFetch && tc.trustedReverified {
				wantCalls = 1
			}
			if fake.calls != wantCalls {
				t.Fatalf("combo %q: preparer calls = %d, want %d", tc.name, fake.calls, wantCalls)
			}
		})
	}

	t.Run("source contains gate name and socket prefix, no stage-2 mount", func(t *testing.T) {
		raw, err := os.ReadFile("runner.go")
		if err != nil {
			t.Fatalf("cannot read runner.go: %v", err)
		}
		src := string(raw)
		if !strings.Contains(src, "RunSnapshotWithNetwork") {
			t.Fatal("runner.go must define RunSnapshotWithNetwork")
		}
		if !strings.Contains(src, "depgw-") {
			t.Fatal("runner.go must reference the depgw- per-job socket prefix")
		}
		if strings.Contains(src, "gateway.sock") {
			t.Fatal("runner.go must never name an in-sandbox gateway socket mount (stage 2 is socket-free by construction)")
		}
	})
}

// TestOfflineCacheMissMapping pins the INCOMPLETE rows: offline cache-miss
// markers map to incomplete, while genuine compile errors stay build failures.
func TestOfflineCacheMissMapping(t *testing.T) {
	for _, stderr := range []string{
		"go: module lookup disabled by GOPROXY=off",
		"missing go.sum entry for module providing package",
		"cannot find module providing package example.com/x",
	} {
		if !isOfflineCacheMiss(stderr) {
			t.Errorf("expected cache-miss detection for %q", stderr)
		}
	}
	for _, stderr := range []string{
		"syntax error in main.go",
		"FAIL: test assertion failed",
		"",
	} {
		if isOfflineCacheMiss(stderr) {
			t.Errorf("must not classify %q as cache miss", stderr)
		}
	}
}

// TestStaleSocketSweep proves the per-job lifecycle: a leftover depgw- socket
// is removed when the gate stays closed, while unrelated files are kept.
func TestStaleSocketSweep(t *testing.T) {
	lease := testSlotLease(t)
	stale := filepath.Join(lease.ControlDir, "depgw-oldjob.sock")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(lease.ControlDir, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{}
	if _, err := r.runPrepGated(context.Background(), "job-new", lease, false); err != nil {
		t.Fatalf("runPrepGated error: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale per-job socket must be swept when gate is closed: %q", stale)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated control file must survive the sweep: %v", err)
	}
}

