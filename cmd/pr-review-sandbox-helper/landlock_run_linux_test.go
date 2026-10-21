//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPositiveControlFailsClosed proves a read-write root that cannot accept a
// create is caught (exit 73) without ever calling restrict_self in-process.
func TestPositiveControlFailsClosed(t *testing.T) {
	writable := t.TempDir()
	if err := positiveControl([]string{writable}); err != nil {
		t.Fatalf("writable root must pass the positive control: %v", err)
	}
	// The control must not leave its probe file behind.
	entries, err := os.ReadDir(writable)
	if err != nil {
		t.Fatalf("read writable root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("positive control must clean up after itself, found %d entries", len(entries))
	}

	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	if err := positiveControl([]string{readOnly}); err == nil {
		t.Fatal("a 0555 root must fail the positive control")
	}
}

// TestNegativeControlFailsClosed proves the negative control distinguishes a
// writable "read-only" root (the security denial → 74) from an ordinary
// denial (a pass), without any Landlock kernel.
func TestNegativeControlFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so a 0555 directory is still writable")
	}

	writable := t.TempDir()
	err := negativeControl([]string{writable})
	if err == nil {
		t.Fatal("a writable read-only root must fail the negative control")
	}
	var typed *errNegativeControlWritable
	if !errors.As(err, &typed) {
		t.Fatalf("want *errNegativeControlWritable, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), writable) {
		t.Errorf("error must name the offending root: %v", err)
	}
	// The probe file must be removed even on the failure path.
	entries, readErr := os.ReadDir(writable)
	if readErr != nil {
		t.Fatalf("read writable root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("negative control must remove its probe file, found %d entries", len(entries))
	}

	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	if err := negativeControl([]string{readOnly}); err != nil {
		t.Fatalf("a denied write is a passing negative control, got %v", err)
	}
}

// TestLandlockRunLinuxSourceShape structurally pins the applier's shape: the
// pinned thread, the two domain syscalls, the pre-exec controls and the final
// exec, with no supervision (T-10-02).
func TestLandlockRunLinuxSourceShape(t *testing.T) {
	raw, err := os.ReadFile("landlock_run_linux.go")
	if err != nil {
		t.Fatalf("read landlock_run_linux.go: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"runtime.LockOSThread()",
		"PR_SET_NO_NEW_PRIVS",
		"SYS_LANDLOCK_RESTRICT_SELF",
		"positiveControl(",
		"negativeControl(",
		"syscall.Exec(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("landlock_run_linux.go must contain %q", want)
		}
	}
	for _, banned := range []string{"exec.Command", "os.StartProcess", "syscall.ForkExec"} {
		if strings.Contains(src, banned) {
			t.Errorf("landlock_run_linux.go must not contain %q: any supervision reintroduces the multi-thread hole", banned)
		}
	}
}

// buildHelper compiles the helper for the given GOOS/GOARCH into dir.
func buildHelper(t *testing.T, dir, goos, goarch string) string {
	t.Helper()
	name := "pr-review-sandbox-helper"
	if goos == "windows" {
		name += ".exe"
	}
	out := filepath.Join(dir, goos+"_"+goarch+"_"+name)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s/%s: %v\n%s", goos, goarch, err, combined)
	}
	return out
}

// TestHelperLandlockRunSubprocess builds the helper once and runs the real
// subprocess contract. On this host Landlock is disabled, so 70 is the
// recorded observation and 0 is accepted for a Landlock-capable host; either
// way the usage code is exact.
func TestHelperLandlockRunSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess build skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent on PATH")
	}
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("/bin/true absent on this host")
	}

	helper := buildHelper(t, t.TempDir(), runtime.GOOS, runtime.GOARCH)

	t.Run("no-args-is-usage", func(t *testing.T) {
		cmd := exec.Command(helper, "landlock-run")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run helper: %v", err)
			}
			code = exitErr.ExitCode()
		}
		if code != 75 {
			t.Fatalf("exit = %d, want 75 (usage); output: %s", code, out)
		}
		if !strings.Contains(string(out), "landlock-run") {
			t.Errorf("usage output must name the subcommand: %s", out)
		}
	})

	t.Run("scratch-rw-root", func(t *testing.T) {
		scratch := t.TempDir()
		cmd := exec.Command(helper, "landlock-run", "--ro", "/", "--rw", scratch, "--", "/bin/true")
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("run helper: %v", err)
			}
			code = exitErr.ExitCode()
		}
		switch code {
		case 0:
			t.Log("observed landlock-run exit 0: this host has a usable Landlock ABI")
		case 70:
			t.Logf("observed landlock-run exit 70 (Landlock unavailable on this host): %s", strings.TrimSpace(string(out)))
			if strings.TrimSpace(string(out)) == "" {
				t.Error("exit 70 must carry a typed reason on stderr")
			}
		default:
			t.Fatalf("exit = %d, want 0 or 70; output: %s", code, out)
		}
	})
}

// TestHelperCrossCompiles proves landlock-run keeps the helper buildable on
// every platform while the rank stays Linux-only.
func TestHelperCrossCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent on PATH")
	}
	dir := t.TempDir()
	for _, target := range [][2]string{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
	} {
		buildHelper(t, dir, target[0], target[1])
	}
}
