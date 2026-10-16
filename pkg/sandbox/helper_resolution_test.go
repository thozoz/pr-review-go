package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHelperFile creates a regular file with the given mode.
func writeHelperFile(t *testing.T, path string, perm os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// resolutionDeps builds a helperResolveDeps over explicit probe results.
func resolutionDeps(env, exe, pathHit string) helperResolveDeps {
	return helperResolveDeps{
		getenv: func(key string) string {
			if key == SandboxHelperPathEnv {
				return env
			}
			return ""
		},
		executable: func() (string, error) {
			if exe == "" {
				return "", errors.New("no executable")
			}
			return exe, nil
		},
		lookPath: func(string) (string, error) {
			if pathHit == "" {
				return "", errors.New("not found on PATH")
			}
			return pathHit, nil
		},
		stat: os.Stat,
	}
}

// TestResolveSandboxHelperPathPrecedence pins the documented
// env → sibling → PATH → identity-guarded self-exec order.
func TestResolveSandboxHelperPathPrecedence(t *testing.T) {
	tmp := t.TempDir()
	envHelper := writeHelperFile(t, filepath.Join(tmp, "env", HelperBinaryName), 0o755)
	exeDir := filepath.Join(tmp, "exe")
	exeServer := writeHelperFile(t, filepath.Join(exeDir, "pr-review-server"), 0o755)
	exeSibling := writeHelperFile(t, filepath.Join(exeDir, HelperBinaryName), 0o755)
	exeOnlyDir := filepath.Join(tmp, "exe-only")
	exeOnlyServer := writeHelperFile(t, filepath.Join(exeOnlyDir, "pr-review-server"), 0o755)
	pathHelper := writeHelperFile(t, filepath.Join(tmp, "path", HelperBinaryName), 0o755)

	cases := []struct {
		name string
		deps helperResolveDeps
		want string
	}{
		{"env-wins-over-sibling-and-path", resolutionDeps(envHelper, exeServer, pathHelper), envHelper},
		{"sibling-wins-over-path", resolutionDeps("", exeServer, pathHelper), exeSibling},
		{"path-used-when-env-and-sibling-absent", resolutionDeps("", exeOnlyServer, pathHelper), pathHelper},
		{"identity-fallback-when-binary-is-the-helper", resolutionDeps("", exeSibling, ""), exeSibling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSandboxHelperPathWith(tc.deps)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}

	// Negative identity guard: a server binary with no sibling and no PATH hit
	// must fail rather than self-exec into a missing subcommand.
	if got, err := resolveSandboxHelperPathWith(resolutionDeps("", exeOnlyServer, "")); err == nil {
		t.Fatalf("expected failure for a non-helper executable, got %q", got)
	}
}

// TestResolveSandboxHelperPathFailsClosed proves resolution never degrades to
// a guessed binary: an explicitly set SANDBOX_HELPER_PATH that is missing or
// non-executable fails even when a valid sibling and PATH entry exist, and an
// exhausted probe set returns a typed ErrConfinementUnavailable.
func TestResolveSandboxHelperPathFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	exeDir := filepath.Join(tmp, "exe")
	exeServer := writeHelperFile(t, filepath.Join(exeDir, "pr-review-server"), 0o755)
	writeHelperFile(t, filepath.Join(exeDir, HelperBinaryName), 0o755)
	pathHelper := writeHelperFile(t, filepath.Join(tmp, "path", HelperBinaryName), 0o755)

	nonExec := writeHelperFile(t, filepath.Join(tmp, "env-nonexec"), 0o644)

	for _, tc := range []struct {
		name string
		env  string
	}{
		{"env-missing", filepath.Join(tmp, "does-not-exist")},
		{"env-non-executable", nonExec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSandboxHelperPathWith(resolutionDeps(tc.env, exeServer, pathHelper))
			if !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("want ErrConfinementUnavailable, got %v", err)
			}
			if !strings.Contains(err.Error(), SandboxHelperPathEnv) {
				t.Errorf("error must name %s: %v", SandboxHelperPathEnv, err)
			}
			if got != "" {
				t.Errorf("failed resolution must not return a path, got %q", got)
			}
		})
	}

	// Every probe empty: fail with the sentinel, never "".
	t.Run("all-probes-empty", func(t *testing.T) {
		got, err := resolveSandboxHelperPathWith(resolutionDeps("", "", ""))
		if !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want an empty path", got)
		}
		if err.Error() == "" || !strings.Contains(err.Error(), HelperBinaryName) {
			t.Errorf("error must name the helper binary: %v", err)
		}
	})
}
