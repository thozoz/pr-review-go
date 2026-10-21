package main

import (
	"io"
	"os"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

// Default operator values for sandbox provisioning. Kept as aliases so
// package main callers and tests share the canonical pkg/sandbox constants.
const (
	defaultSandboxSetupImage    = sandbox.DefaultSandboxSetupImage
	defaultSandboxSetupSlotDir  = sandbox.DefaultSandboxSetupSlotDir
	defaultSandboxSetupSlotSize = sandbox.DefaultSandboxSetupSlotSize
	defaultSandboxControlDir    = sandbox.DefaultSandboxControlDir
)

// runSetupSandbox provisions sandbox prerequisites on Linux and returns a
// process exit code. The shared engine lives in pkg/sandbox/setup.go so both
// pr-review-server and pr-review-go expose identical behavior.
func runSetupSandbox(cfg *config.Config, opts sandbox.SetupOptions) int {
	return sandbox.RunSetup(cfg, opts, os.Stdout, os.Stderr)
}

// isMounted reports whether dir is a mount point per /proc/self/mountinfo.
func isMounted(dir string) (bool, error) {
	return sandbox.IsMounted(dir)
}

// isMountedFromReader reports whether dir appears in mountinfo data from r.
func isMountedFromReader(r io.Reader, dir string) (bool, error) {
	return sandbox.IsMountedFromReader(r, dir)
}

// firstNonEmpty returns the first non-empty string, or "" when all are empty.
func firstNonEmpty(vals ...string) string {
	return sandbox.FirstNonEmpty(vals...)
}
