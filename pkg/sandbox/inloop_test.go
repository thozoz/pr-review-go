package sandbox

// Phase 06-03 (D-03): direct plus podman in-loop parity, rejection, and
// advisory-only authority. Mock-based: no Podman daemon, no network. The one
// live-toolchain test (go build in an empty stdlib-only module) skips when
// no Go binary is on PATH.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// podmanGateAccepts mirrors InLoopExecCommand's workDir-free gate order
// (element checks, head allow-list, union-row membership) without executing.
func podmanGateAccepts(argv []string) bool {
	if err := validateInLoopArgv(argv); err != nil {
		return false
	}
	if len(argv) == 0 || !inLoopHeadAllowed(argv[0]) {
		return false
	}
	return inLoopArgvAllowed(argv, inLoopAllowedUnion())
}

// directGateAccepts mirrors InLoopCommand's gate order (element checks plus
// per-project row membership) without executing.
func directGateAccepts(workDir string, argv []string) bool {
	if err := validateInLoopArgv(argv); err != nil {
		return false
	}
	proj := detectDirectProject(workDir)
	if proj == nil {
		return false
	}
	return inLoopArgvAllowed(argv, inLoopAllowedCommands(proj))
}

func writeMarker(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestInLoop_ParityMatrix(t *testing.T) {
	cases := []struct {
		name         string
		marker       string
		content      string
		firstRow     []string
		wantRowCount int
	}{
		{name: "go", marker: "go.mod", content: "module example.com/tmp\n\ngo 1.22\n", firstRow: []string{"go", "build", "./..."}, wantRowCount: 3},
		{name: "npm", marker: "package.json", content: "{}\n", firstRow: []string{"npm", "test"}, wantRowCount: 1},
		{name: "pnpm", marker: "pnpm-lock.yaml", content: "lockfileVersion: 9\n", firstRow: []string{"pnpm", "test"}, wantRowCount: 1},
		{name: "pip", marker: "requirements.txt", content: "pytest\n", firstRow: []string{"pytest"}, wantRowCount: 5},
		{name: "cargo", marker: "Cargo.toml", content: "[package]\nname = \"tmp\"\n", firstRow: []string{"cargo", "test"}, wantRowCount: 1},
		{name: "maven", marker: "pom.xml", content: "<project/>\n", firstRow: []string{"mvn", "-q", "test"}, wantRowCount: 1},
		{name: "make", marker: "Makefile", content: "test:\n\ttrue\n", firstRow: []string{"make", "test"}, wantRowCount: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMarker(t, dir, tc.marker, tc.content)

			proj := detectDirectProject(dir)
			if proj == nil {
				t.Fatalf("detectDirectProject returned nil for marker %s", tc.marker)
			}
			allowed := inLoopAllowedCommands(proj)
			if len(allowed) != tc.wantRowCount {
				t.Fatalf("expected %d allowed rows for %s, got %v", tc.wantRowCount, tc.marker, allowed)
			}
			if !inLoopArgvAllowed(tc.firstRow, allowed) {
				t.Fatalf("first row %v not in allowed set %v", tc.firstRow, allowed)
			}

			// Parity: every direct row must pass the podman gate too, and
			// every row must be a member of the shared union (drift pin).
			union := inLoopAllowedUnion()
			for _, row := range allowed {
				if !directGateAccepts(dir, row) {
					t.Fatalf("direct gate rejects its own matrix row %v", row)
				}
				if !podmanGateAccepts(row) {
					t.Fatalf("podman gate diverges on direct matrix row %v", row)
				}
				if !inLoopArgvAllowed(row, union) {
					t.Fatalf("union drifted: matrix row %v not in union", row)
				}
			}
		})
	}

	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		if proj := detectDirectProject(dir); proj != nil {
			t.Fatalf("expected nil project for empty dir, got %+v", proj)
		}
		if got := inLoopAllowedCommands(nil); len(got) != 0 {
			t.Fatalf("expected zero allowed rows for nil project, got %v", got)
		}
		sample := []string{"go", "build", "./..."}
		if directGateAccepts(dir, sample) {
			t.Fatalf("direct gate accepts %v with no project", sample)
		}
		_, _, code, err := InLoopCommand(context.Background(), dir, sample, 0, 0)
		if err == nil || !strings.Contains(err.Error(), "in-loop command not in project matrix") {
			t.Fatalf("expected matrix mismatch, got code=%d err=%v", code, err)
		}
		if code != -1 {
			t.Fatalf("expected exit -1 on mismatch, got %d", code)
		}
	})
}

func TestInLoop_MatrixMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod", "module example.com/tmp\n\ngo 1.22\n")

	// Not matrix rows for a go project: wrong subcommand, foreign binary,
	// install argv (network), and absolute-path binary.
	bad := [][]string{
		{"go", "version"},
		{"curl", "https://example.com"},
		{"npm", "ci"},
		{"/bin/go", "build", "./..."},
		{"go", "build", "./...", "extra", "a", "b", "c", "d", "e"},
	}
	for _, argv := range bad {
		if directGateAccepts(dir, argv) {
			t.Fatalf("direct gate accepts non-matrix argv %v", argv)
		}
		if podmanGateAccepts(argv) {
			t.Fatalf("podman gate accepts non-matrix argv %v", argv)
		}
		_, _, code, err := InLoopCommand(context.Background(), dir, argv, time.Minute, 6000)
		if err == nil || !strings.Contains(err.Error(), "in-loop command not in project matrix") {
			t.Fatalf("argv %v: expected matrix mismatch, got code=%d err=%v", argv, code, err)
		}
		if code != -1 {
			t.Fatalf("argv %v: expected exit -1, got %d (err=%v)", argv, code, err)
		}
	}

	// Empty argv is rejected before any matrix lookup.
	if _, _, _, err := InLoopCommand(context.Background(), dir, nil, 0, 0); err == nil {
		t.Fatalf("expected empty-argv rejection")
	}
}

func TestInLoop_ShellMetacharRejected(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod", "module example.com/tmp\n\ngo 1.22\n")

	evil := [][]string{
		{"go", "test", "a;b"},
		{"go", "test", "a&b"},
		{"go", "test", "a|b"},
		{"go", "test", "$HOME"},
		{"go", "test", "`id`"},
		{"go", "test", "a\\b"},
		{"go", "test", "a\nb"},
		{"go", "test", ".."},
		{"go", "test", "../escape"},
		{"go", "test", "sub/../../escape"},
	}
	for _, argv := range evil {
		if err := validateInLoopArgv(argv); err == nil {
			t.Fatalf("element gate accepts hostile argv %q", argv)
		}
		if _, _, _, err := InLoopCommand(context.Background(), dir, argv, time.Minute, 6000); err == nil {
			t.Fatalf("InLoopCommand executed hostile argv %q", argv)
		}
		if podmanGateAccepts(argv) {
			t.Fatalf("podman gate accepts hostile argv %q", argv)
		}
	}
}

func TestInLoop_MissingBinaryUnavailable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent; nothing to shadow")
	}
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod", "module example.com/tmp\n\ngo 1.22\n")
	matrixArgv := []string{"go", "build", "./..."}
	if !directGateAccepts(dir, matrixArgv) {
		t.Fatalf("fixture argv %v must pass the direct gate", matrixArgv)
	}

	// Empty PATH shadows every toolchain binary; no exec is attempted.
	t.Setenv("PATH", t.TempDir())

	_, _, code, err := InLoopCommand(context.Background(), dir, matrixArgv, time.Minute, 6000)
	if err == nil || !strings.Contains(err.Error(), "toolchain not installed") {
		t.Fatalf("expected toolchain-not-installed unavailable, got code=%d err=%v", code, err)
	}
	if !errors.Is(err, ErrInLoopToolchainMissing) {
		t.Fatalf("expected ErrInLoopToolchainMissing wrap, got %v", err)
	}

	r := &Runner{}
	_, _, pcode, perr := r.InLoopExecCommand(context.Background(), "inloop-test-container", matrixArgv, time.Minute, 6000)
	if perr == nil || !strings.Contains(perr.Error(), "toolchain not installed") {
		t.Fatalf("expected podman toolchain-not-installed unavailable, got code=%d err=%v", pcode, perr)
	}
	if !errors.Is(perr, ErrInLoopToolchainMissing) {
		t.Fatalf("expected ErrInLoopToolchainMissing wrap on podman path, got %v", perr)
	}
}

func TestInLoop_PodmanExitMapping(t *testing.T) {
	if code, err := mapInLoopPodmanExit("cid", "go", 127); code != 127 || err == nil ||
		!strings.Contains(err.Error(), "toolchain not installed") || !errors.Is(err, ErrInLoopToolchainMissing) {
		t.Fatalf("exit 127 must map to unavailable, got code=%d err=%v", code, err)
	}
	if code, err := mapInLoopPodmanExit("cid", "go", 1); code != 1 || err != nil {
		t.Fatalf("exit 1 must stay an observation, got code=%d err=%v", code, err)
	}

	r := &Runner{}
	if _, _, _, err := r.InLoopExecCommand(context.Background(), "", []string{"go", "test", "-race", "./..."}, 0, 0); err == nil {
		t.Fatalf("expected container-ID requirement error")
	}
	if _, _, code, err := r.InLoopExecCommand(context.Background(), "cid", []string{"curl", "x"}, 0, 0); err == nil ||
		!strings.Contains(err.Error(), "in-loop command not in project matrix") || code != -1 {
		t.Fatalf("expected podman matrix mismatch, got code=%d err=%v", code, err)
	}
}

func TestInLoop_OutputCapMarker(t *testing.T) {
	const capBytes = 6000
	c := NewLimitedCollector(capBytes)
	if _, err := c.Write([]byte(strings.Repeat("o", capBytes+1000))); err != nil {
		t.Fatal(err)
	}
	out := formatInLoopOutput(c, capBytes)
	if !strings.Contains(out, "...[truncated: command output exceeds 6000 bytes]...") {
		t.Fatalf("cap marker missing in truncated output")
	}

	small := NewLimitedCollector(capBytes)
	if _, err := small.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got := formatInLoopOutput(small, capBytes); got != "ok" {
		t.Fatalf("untruncated output must pass through verbatim, got %q", got)
	}
}

func TestInLoop_DirectExecSmoke(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent on PATH")
	}
	// Rank availability: on a host with no usable confinement rank the command
	// is refused by design, so this live smoke skips with the typed reason
	// instead of failing. The fail-closed refusal itself is pinned by
	// TestInLoopCommandRoutesThroughConfinementDispatch.
	if _, err := ResolveConfinementRank(context.Background()); err != nil {
		t.Skipf("no confinement rank available on this host: %v", err)
	}
	dir := t.TempDir()
	writeMarker(t, dir, "go.mod", "module example.com/inloopsmoke\n\ngo 1.22\n")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code, err := InLoopCommand(context.Background(), dir, []string{"go", "build", "./..."}, 2*time.Minute, 0,
		WithDirectRoots(DirectRoots{WorkRoot: directTestRoot(t), CacheRoot: directTestRoot(t)}))
	if err != nil {
		t.Fatalf("matrix exec failed: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stdout=%q stderr=%q)", code, stdout, stderr)
	}
	if strings.Contains(stdout, "[truncated:") || strings.Contains(stderr, "[truncated:") {
		t.Fatalf("small output must not carry a truncation marker")
	}
}

func TestInLoop_UnavailableNeverRetried(t *testing.T) {
	// Pins the server-side contract jobs.go isRetryableVerifyStatus to the
	// in-loop unavailable shape: StatusUnavailable must never appear in the
	// retryable set, so a missing toolchain completes honestly instead of
	// burning the 3x verify-retry budget.
	raw, err := os.ReadFile(filepath.Join("..", "server", "jobs.go"))
	if err != nil {
		t.Fatalf("cannot read server jobs.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func isRetryableVerifyStatus")
	if start < 0 {
		t.Fatalf("isRetryableVerifyStatus not found in jobs.go")
	}
	// Line-ending agnostic: the body ends at the first line holding only }.
	tail := src[start+1:]
	end := strings.Index(tail, "\n}")
	if end < 0 {
		t.Fatalf("cannot delimit isRetryableVerifyStatus body")
	}
	body := src[start : start+1+end]
	for _, want := range []string{"StatusBuildFailed", "StatusTestFailed", "StatusTimeout"} {
		if !strings.Contains(body, want) {
			t.Fatalf("retryable set lost %s: %q", want, body)
		}
	}
	if strings.Contains(body, "StatusUnavailable") {
		t.Fatalf("StatusUnavailable must never be retryable: %q", body)
	}
}

func TestInLoop_AdvisoryOnlyNoPushCallSites(t *testing.T) {
	// No call path from the in-loop entries to the push transport: only the
	// outer RunSnapshot green authorizes push. Test files are excluded since
	// this assertion itself names the symbol.
	var hits []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), "CommitFilesAtExpectedHead") {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("push call sites inside pkg/sandbox: %v", hits)
	}
}

// TestInLoop_WorkDirValidated pins 09-REVIEW IN-05: a caller-supplied work
// directory is canonicalized and must exist as a directory before any rank
// sees it; a relative escape, missing path or regular file is refused.
func TestInLoop_WorkDirValidated(t *testing.T) {
	roots := WithDirectRoots(DirectRoots{WorkRoot: directTestRoot(t), CacheRoot: directTestRoot(t)})

	filePath := filepath.Join(t.TempDir(), "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		workDir string
	}{
		{"empty", ""},
		{"missing", filepath.Join(t.TempDir(), "does-not-exist")},
		{"regular-file", filePath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, code, err := InLoopCommand(context.Background(), tc.workDir, []string{"go", "test", "./..."}, time.Minute, 0, roots)
			if err == nil {
				t.Fatalf("workDir %q must be refused", tc.workDir)
			}
			if code != -1 {
				t.Fatalf("refusal code = %d, want -1", code)
			}
			if !strings.Contains(err.Error(), "work directory") {
				t.Fatalf("refusal must name the work directory: %v", err)
			}
		})
	}
}
