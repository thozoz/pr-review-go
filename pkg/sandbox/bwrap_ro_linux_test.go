//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nonShadowedTestDir creates a 0700 directory outside the mount the bwrap-ro
// argv shadows with `--tmpfs /tmp`. A sentinel under host /tmp would be
// invisible inside the sandbox, so an out-of-allow-list write would fail with
// ENOENT instead of EROFS — exactly the false positive this helper prevents.
func nonShadowedTestDir(t *testing.T) string {
	t.Helper()
	if dir, err := os.MkdirTemp("/var/tmp", "pr-review-ro-test-*"); err == nil {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("chmod probe dir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		if dir, err := os.MkdirTemp(cache, "pr-review-ro-test-*"); err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	t.Skip("no non-shadowed temporary directory (/var/tmp and the user cache dir are both unwritable): a sentinel under host /tmp is hidden by --tmpfs /tmp and cannot prove EROFS")
	return ""
}

// skipBwrapRO skips live tests on a host where the bwrap-ro rank is not
// available, so the suite never fails off-target. The rank decision itself is
// pinned by TestBwrapROLiveProbe.
func skipBwrapRO(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap absent: the bwrap-ro rank cannot be proven on this host: %v", err)
	}
	if err := AttestBwrapRO(context.Background()); err != nil {
		t.Skipf("bwrap-ro rank unavailable on this host: %v", err)
	}
}

// runBwrapRO executes one command through the production builder and returns
// the stage result. The env is nil: the rank carries its environment inside
// the wrapper's --clearenv/--setenv block.
func runBwrapRO(t *testing.T, ctx context.Context, timeout time.Duration, paths DirectJobPaths, command []string) *StageResult {
	t.Helper()
	argv, err := BuildBwrapROStageArgv(command, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("build bwrap-ro argv: %v", err)
	}
	return runDirectStage(ctx, timeout, paths.WorkspaceDir, "bwrap-ro-probe", argv, nil)
}

// TestBwrapROLiveProbe asserts the real-shape rank probe succeeds on a host
// where bubblewrap is installed (Phase 9 success criterion 5). Where bwrap is
// absent the test skips; the rest of the live suite skips off this probe.
func TestBwrapROLiveProbe(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap absent, no rank to prove: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := AttestBwrapRO(ctx); err != nil {
		t.Fatalf("bwrap is installed but the read-only-host rank does not attest: %v", err)
	}
}

// writeProbeSource reports the per-target errno of a write attempt, so the
// test asserts an exact errno rather than matching English error text.
const writeProbeSource = `package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	if len(os.Args) != 7 {
		fmt.Fprintln(os.Stderr, "usage: writeprobe <sentinel> <work> <cache> <tmp> <home> <usr>")
		os.Exit(2)
	}
	sentinel, work, cache, tmp, home, usr := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]
	ok := true
	ok = checkErrno("host-sentinel-outside-allowlist", sentinel, syscall.EROFS) && ok
	ok = checkAnyErr("usr", usr) && ok
	ok = checkOK("workspace", work) && ok
	ok = checkOK("cache", cache) && ok
	ok = checkOK("tmp", tmp) && ok
	ok = checkOK("home", home) && ok
	if !ok {
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "WRITE-MATRIX-OK")
}

func checkErrno(label, path string, want syscall.Errno) bool {
	err := os.WriteFile(path, []byte("probe"), 0o600)
	if err == nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: write to %s unexpectedly succeeded\n", label, path)
		return false
	}
	if !errors.Is(err, want) {
		fmt.Fprintf(os.Stderr, "FAIL %s: write to %s: got %v, want %v\n", label, path, err, want)
		return false
	}
	fmt.Fprintf(os.Stderr, "OK %s: %v\n", label, err)
	return true
}

func checkAnyErr(label, path string) bool {
	err := os.WriteFile(path, []byte("probe"), 0o600)
	if err == nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: write to %s unexpectedly succeeded\n", label, path)
		return false
	}
	fmt.Fprintf(os.Stderr, "OK %s: %v\n", label, err)
	return true
}

func checkOK(label, dir string) bool {
	target := filepath.Join(dir, "write-probe-"+label)
	if err := os.WriteFile(target, []byte("probe"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: write to %s: %v\n", label, target, err)
		return false
	}
	if _, err := os.Stat(target); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: wrote %s but it is not readable: %v\n", label, target, err)
		return false
	}
	fmt.Fprintf(os.Stderr, "OK %s: wrote %s\n", label, target)
	return true
}
`

// TestBwrapROWriteMatrix proves Phase 9 criterion 2: inside the rank a write
// outside the allow-list fails with EROFS and leaves the host file unchanged,
// while writes inside the workspace, cache, job tmp and home succeed. The
// unwrapped positive control first proves the same host path is writable, so
// the EROFS assertion is not vacuous.
func TestBwrapROWriteMatrix(t *testing.T) {
	skipBwrapRO(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent: cannot build the in-sandbox write probe")
	}

	sentinelDir := nonShadowedTestDir(t)
	sentinel := filepath.Join(sentinelDir, "host-sentinel")
	if err := os.WriteFile(sentinel, []byte("sentinel-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Positive control, unwrapped: the sentinel must be writable on the host.
	if err := os.WriteFile(sentinel, []byte("control-content"), 0o600); err != nil {
		t.Fatalf("positive control: host sentinel must be writable outside the sandbox: %v", err)
	}
	if err := os.WriteFile(sentinel, []byte("sentinel-content"), 0o600); err != nil {
		t.Fatal(err)
	}

	work := writeDirectModule(t, map[string]string{
		"go.mod":                 "module example.com/writeprobe\n\ngo 1.24\n",
		"cmd/writeprobe/main.go": writeProbeSource,
	})
	paths := provisionTestJobPaths(t)
	paths.WorkspaceDir = work

	script := "set -e\n" +
		"go build -o bin/writeprobe ./cmd/writeprobe\n" +
		"./bin/writeprobe \"$1\" \"$2\" \"$3\" \"$4\" \"$5\" /usr/.pr-review-write-probe\n"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := runBwrapRO(t, ctx, 4*time.Minute, paths, []string{
		"/bin/sh", "-c", script, "sh",
		sentinel, work, paths.CacheDir, paths.TmpDir, paths.HomeDir,
	})
	if !res.Passed {
		t.Fatalf("write matrix failed (exit %d):\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "WRITE-MATRIX-OK") {
		t.Fatalf("write matrix did not report success: stderr=%s", res.Stderr)
	}

	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "sentinel-content" {
		t.Fatalf("host sentinel must be unmodified, got %q (%v)", contents, err)
	}
	entries, err := os.ReadDir(sentinelDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host-sentinel" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no file may be created beside the host sentinel, found %v", names)
	}
}

// TestBwrapROHostTmpIsPrivate proves --tmpfs /tmp gives the stage a private
// tmp: a host /tmp marker is invisible inside, a write to the same path inside
// succeeds, and the host file is never touched.
func TestBwrapROHostTmpIsPrivate(t *testing.T) {
	skipBwrapRO(t)

	marker := filepath.Join(os.TempDir(), fmt.Sprintf("pr-review-ro-marker-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(marker, []byte("host-tmp-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })

	paths := provisionTestJobPaths(t)
	script := "set -e\n" +
		"if [ -e \"$1\" ]; then echo 'host /tmp leaked into the sandbox' >&2; exit 1; fi\n" +
		"echo inside > \"$1\"\n" +
		"test \"$(cat \"$1\")\" = inside\n"

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res := runBwrapRO(t, ctx, time.Minute, paths, []string{"/bin/sh", "-c", script, "sh", marker})
	if !res.Passed {
		t.Fatalf("private-tmp stage failed (exit %d):\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "host-tmp-marker" {
		t.Fatalf("host /tmp marker must be untouched, got %q (%v)", contents, err)
	}
}

// TestBwrapROBuildsAndExecutesWorkspaceBinary proves Phase 9 criterion 3: a
// freshly built binary inside the workspace executes from there (EXECUTE
// right) and a write-temp-then-rename succeeds.
func TestBwrapROBuildsAndExecutesWorkspaceBinary(t *testing.T) {
	skipBwrapRO(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent: cannot build the workspace binary")
	}

	work := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/bwraprouexec\n\ngo 1.24\n",
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"bwrap-ro-exec-ok\") }\n",
	})
	paths := provisionTestJobPaths(t)
	paths.WorkspaceDir = work

	script := "set -e\n" +
		"go build -o bin/probe .\n" +
		"out=$(./bin/probe)\n" +
		"test \"$out\" = bwrap-ro-exec-ok\n" +
		"mv bin/probe bin/probe.renamed\n" +
		"out2=$(./bin/probe.renamed)\n" +
		"test \"$out2\" = bwrap-ro-exec-ok\n"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := runBwrapRO(t, ctx, 4*time.Minute, paths, []string{"/bin/sh", "-c", script, "sh"})
	if !res.Passed {
		t.Fatalf("compile/execute/rename stage failed (exit %d):\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(work, "bin", "probe.renamed")); err != nil {
		t.Fatalf("renamed workspace binary missing on host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "bin", "probe")); !os.IsNotExist(err) {
		t.Fatalf("the original binary must have been renamed away, stat err = %v", err)
	}
}
