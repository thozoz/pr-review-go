package sandbox

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// PlatformComponents builds the production container backend and slot manager
// for the given config, or returns nils when isolated execution is unavailable.
//
// Fail-closed contract: any missing prerequisite (non-Linux platform, sandbox
// disabled, image or slot dir unconfigured, slot manager construction error)
// yields (nil, nil). Callers pass the results straight into NewPlatformRunner,
// which falls back to APISnapshotSource / StatusUnavailable in that case —
// unverified code is never executed on the host and never pushed.
//
// Resource mapping: backend limits (CPUs, memory, pids, timeouts) come from
// the sandbox fields of cfg; NewPodmanBackend applies its own safe defaults
// for non-positive values.
func PlatformComponents(cfg *config.Config) (*PodmanBackend, SlotManager) {
	backend := PodmanBackendForConfig(cfg)
	sm := SlotManagerForConfig(cfg)
	if backend == nil || sm == nil {
		return nil, nil
	}
	return backend, sm
}

// PodmanBackendForConfig returns a configured backend, or nil when isolated
// execution is unavailable. Backend construction itself is OS-independent; the
// Linux gate keeps every PlatformRunner call site consistent.
func PodmanBackendForConfig(cfg *config.Config) *PodmanBackend {
	if runtime.GOOS != "linux" {
		return nil
	}
	if cfg == nil || !cfg.EnableSandbox {
		return nil
	}
	// Direct mode verifies on the host; no container backend is built.
	if cfg.SandboxMode == config.SandboxModeDirect {
		return nil
	}
	if cfg.SandboxImage == "" || cfg.SandboxSlotDir == "" {
		return nil
	}
	return NewPodmanBackend(PodmanConfig{
		ImageDigest:         cfg.SandboxImage,
		CPUs:                cfg.SandboxCPUs,
		MemoryBytes:         cfg.SandboxMemoryBytes,
		PidsLimit:           cfg.SandboxPidsLimit,
		TimeoutStage:        cfg.SandboxTimeoutExecution,
		TimeoutClean:        cfg.SandboxTimeoutCleanup,
		AllowRootfulSandbox: cfg.AllowRootfulSandbox,
	})
}

// DirectRunnerForConfig builds the containerless runner for pre-isolated
// environments. Unlike the Podman path it needs no image, slot, or Linux
// gate — only an enabled sandbox in direct mode.
func DirectRunnerForConfig(cfg *config.Config) *DirectRunner {
	if cfg == nil || !cfg.EnableSandbox {
		return nil
	}
	if cfg.SandboxMode != config.SandboxModeDirect {
		return nil
	}
	return NewDirectRunner(cfg.SandboxTimeoutExecution)
}

// SlotManagerForConfig returns a Linux slot manager, or nil when isolated
// execution is unavailable or the manager cannot be constructed.
func SlotManagerForConfig(cfg *config.Config) SlotManager {
	if runtime.GOOS != "linux" {
		return nil
	}
	if cfg == nil || !cfg.EnableSandbox {
		return nil
	}
	// Direct mode verifies in place; no slot leasing is used.
	if cfg.SandboxMode == config.SandboxModeDirect {
		return nil
	}
	if cfg.SandboxImage == "" || cfg.SandboxSlotDir == "" {
		return nil
	}
	controlDir := cfg.SandboxControlDir
	if controlDir == "" {
		controlDir = filepath.Join(os.TempDir(), "pr-review-sandbox-control")
	}
	sm, err := NewLinuxSlotManager(cfg.SandboxSlotDir, controlDir)
	if err != nil {
		return nil
	}
	if os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "1" || os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "true" {
		sm.SkipMountChecks = true
	}
	return sm
}
