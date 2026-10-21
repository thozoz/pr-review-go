package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HelperBinaryName is the published name of the trusted confinement helper.
// The server never assumes a path: it resolves this name through
// ResolveSandboxHelperPath, and a resolution failure degrades the landlock
// rank to unavailable rather than executing the target unwrapped (T-10-04).
const HelperBinaryName = "pr-review-sandbox-helper"

// SandboxHelperPathEnv is the operator override for the helper's location.
// When it is set it is authoritative: a missing or non-executable value fails
// closed instead of silently falling through to a different binary, because an
// operator who pinned a path must never get a different applier (T-10-05).
const SandboxHelperPathEnv = "SANDBOX_HELPER_PATH"

// helperResolveDeps is the resolution seam: every probe is injectable so
// precedence and fail-closed behaviour are testable without touching the real
// filesystem or PATH.
type helperResolveDeps struct {
	getenv     func(string) string
	executable func() (string, error)
	lookPath   func(string) (string, error)
	stat       func(string) (os.FileInfo, error)
}

// defaultHelperResolveDeps is the production resolution environment.
func defaultHelperResolveDeps() helperResolveDeps {
	return helperResolveDeps{
		getenv:     os.Getenv,
		executable: os.Executable,
		lookPath:   exec.LookPath,
		stat:       os.Stat,
	}
}

// ResolveSandboxHelperPath resolves the trusted helper binary. Every failure
// wraps ErrConfinementUnavailable so the rank chain refuses the run instead of
// degrading to an unconfined execution.
func ResolveSandboxHelperPath() (string, error) {
	return resolveSandboxHelperPathWith(defaultHelperResolveDeps())
}

// resolveSandboxHelperPathWith implements the documented precedence:
//
//  1. SANDBOX_HELPER_PATH (authoritative when set)
//  2. a sibling of os.Executable() (the release/npm co-location contract)
//  3. exec.LookPath (a PATH install)
//  4. identity-guarded self-exec (only when this binary IS the helper)
func resolveSandboxHelperPathWith(deps helperResolveDeps) (string, error) {
	getenv := deps.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	executable := deps.executable
	if executable == nil {
		executable = func() (string, error) { return "", fmt.Errorf("os.Executable unavailable") }
	}
	lookPath := deps.lookPath
	if lookPath == nil {
		lookPath = func(string) (string, error) { return "", fmt.Errorf("LookPath unavailable") }
	}
	stat := deps.stat
	if stat == nil {
		stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	}

	// 1. Explicit operator override. Authoritative: never fall through.
	if envPath := strings.TrimSpace(getenv(SandboxHelperPathEnv)); envPath != "" {
		fi, err := stat(envPath)
		if err != nil {
			return "", fmt.Errorf("%w: %s=%q is not readable: %v",
				ErrConfinementUnavailable, SandboxHelperPathEnv, envPath, err)
		}
		if !looksExecutable(fi) {
			return "", fmt.Errorf("%w: %s=%q is not a regular executable file",
				ErrConfinementUnavailable, SandboxHelperPathEnv, envPath)
		}
		return envPath, nil
	}

	// 2. Sibling of the running server binary. This is where goreleaser
	// archives and the npm platform packages place the helper. A binary that
	// already IS the helper skips this probe and takes the identity fallback
	// below, which keeps the two branches distinct.
	exePath, exeErr := executable()
	exeIsHelper := exeErr == nil && filepath.Base(exePath) == HelperBinaryName
	if exeErr == nil && exePath != "" && !exeIsHelper {
		sibling := filepath.Join(filepath.Dir(exePath), HelperBinaryName)
		if fi, err := stat(sibling); err == nil && looksExecutable(fi) {
			return sibling, nil
		}
	}

	// 3. PATH.
	if found, err := lookPath(HelperBinaryName); err == nil && found != "" {
		if fi, statErr := stat(found); statErr == nil && looksExecutable(fi) {
			return found, nil
		}
	}

	// 4. Identity-guarded self-exec. Only a binary that IS the helper may
	// re-exec itself: `pr-review-go`/`pr-review-server` deliberately contain no
	// applier, so allowing them here would turn a missing helper into a
	// missing-subcommand failure instead of an honest rank-unavailable.
	if exeIsHelper {
		if fi, err := stat(exePath); err == nil && looksExecutable(fi) {
			return exePath, nil
		}
	}

	exeDescription := "<unavailable>"
	if exeErr == nil && exePath != "" {
		exeDescription = exePath
	}
	return "", fmt.Errorf(
		"%w: %s is unset and no %s was found next to %q or on PATH",
		ErrConfinementUnavailable, SandboxHelperPathEnv, HelperBinaryName, exeDescription)
}

// looksExecutable reports whether fi is a regular file with at least one
// execute bit. It is intentionally portable (no unix.Access) so the resolver
// compiles and behaves on every platform.
func looksExecutable(fi os.FileInfo) bool {
	if fi == nil {
		return false
	}
	return fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
