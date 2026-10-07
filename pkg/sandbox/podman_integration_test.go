package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// TestPodmanTracer verifies real Podman rootless isolation against the deployed runtime.
// It is opt-in via PR_REVIEW_SANDBOX_INTEGRATION=1.
// When enabled, it verifies that missing prerequisites fail closed, and if operator-provisioned
// storage slots and images are available, executes live container verification probes.
func TestPodmanTracer(t *testing.T) {
	if os.Getenv("PR_REVIEW_SANDBOX_INTEGRATION") != "1" {
		t.Skip("skipping live deployment tracer: set PR_REVIEW_SANDBOX_INTEGRATION=1 to enable")
	}

	// 1. Verify fail-closed behavior for missing/invalid protections
	t.Run("PrerequisiteEnforcement", func(t *testing.T) {
		// Verify missing image digest fails closed
		backendNoImage := NewPodmanBackend(PodmanConfig{})
		if err := backendNoImage.Attest(context.Background()); err == nil {
			t.Fatalf("expected Attest to fail when ImageDigest is missing")
		}

		// Verify unmounted normal directory fails slot mount validation
		unmountedSlotDir := t.TempDir()
		controlDir := t.TempDir()
		slotMgr, err := NewLinuxSlotManager(unmountedSlotDir, controlDir)
		if err != nil {
			t.Fatalf("failed to create slot manager: %v", err)
		}
		if _, err := slotMgr.ValidateSlotMount(unmountedSlotDir); err == nil {
			t.Fatalf("expected ValidateSlotMount to fail on normal unmounted directory")
		}

		// Verify quarantined slot refuses acquisition
		slotMgr.SkipMountChecks = true
		if err := slotMgr.QuarantineSlot(unmountedSlotDir, "test failure"); err != nil {
			t.Fatalf("failed to quarantine slot: %v", err)
		}
		if _, err := slotMgr.AcquireSlot(context.Background(), "job-fail"); err == nil || !strings.Contains(err.Error(), "quarantined") {
			t.Fatalf("expected AcquireSlot to fail on quarantined slot, got: %v", err)
		}

		// Verify invalid/prefix commit SHAs fail closed
		invalidSnap := &Snapshot{CommitSHA: "abc1234", SourceDir: t.TempDir()}
		if err := invalidSnap.Validate(); err == nil {
			t.Fatalf("expected short prefix commit SHA to fail validation")
		}

		// Verify escaping symlinks fail closed
		outside := t.TempDir()
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		escapingSnap := &Snapshot{
			CommitSHA: "0123456789abcdef0123456789abcdef01234567",
			SourceDir: source,
		}
		if err := escapingSnap.Validate(); err == nil {
			t.Fatalf("expected escaping symlink to fail validation")
		}
	})

	// 2. Live execution against operator-provisioned environment
	t.Run("LiveExecution", func(t *testing.T) {
		podmanPath, err := exec.LookPath("podman")
		if err != nil {
			t.Logf("Podman binary not found in PATH: %v", err)
			return
		}

		imageDigest := os.Getenv("PR_REVIEW_SANDBOX_IMAGE")
		slotDir := os.Getenv("PR_REVIEW_SLOT_DIR")
		if imageDigest == "" || slotDir == "" {
			t.Logf("Operator setup required: set PR_REVIEW_SANDBOX_IMAGE and PR_REVIEW_SLOT_DIR to run live container probes")
			t.Logf("See 01-USER-SETUP.md for administrator and operator provisioning instructions")
			return
		}

		controlDir := os.Getenv("PR_REVIEW_CONTROL_DIR")
		if controlDir == "" {
			controlDir = t.TempDir()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		slotMgr, err := NewLinuxSlotManager(slotDir, controlDir)
		if err != nil {
			t.Fatalf("failed to initialize slot manager: %v", err)
		}

		mountID, err := slotMgr.ValidateSlotMount(slotDir)
		if err != nil {
			t.Fatalf("operator slot %q failed validation: %v", slotDir, err)
		}
		t.Logf("Operator slot verified: %q (mount ID %d)", slotDir, mountID)

		podmanCfg := PodmanConfig{
			BinaryPath:   podmanPath,
			ImageDigest:  imageDigest,
			CPUs:         2.0,
			MemoryBytes:  2147483648,
			PidsLimit:    256,
			TimeoutStage: 5 * time.Minute,
			TimeoutClean: 15 * time.Second,
		}
		backend := NewPodmanBackend(podmanCfg)

		if err := backend.Attest(ctx); err != nil {
			t.Fatalf("podman capability attestation failed: %v", err)
		}
		t.Logf("Podman capability attestation PASSED")

		// Prepare benign Go fixture
		fixtureDir := t.TempDir()
		goMod := "module tracer.test/fixture\n\ngo 1.22\n"
		if err := os.WriteFile(filepath.Join(fixtureDir, "go.mod"), []byte(goMod), 0644); err != nil {
			t.Fatal(err)
		}
		code := "package fixture\n\nfunc Add(a, b int) int { return a + b }\n"
		if err := os.WriteFile(filepath.Join(fixtureDir, "fixture.go"), []byte(code), 0644); err != nil {
			t.Fatal(err)
		}
		testCode := `package fixture

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2, 3) != 5")
	}
}

func TestProbe_NetworkIsolation(t *testing.T) {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	_, err := d.Dial("tcp", "1.1.1.1:80")
	if err == nil {
		t.Fatalf("security violation: network connection succeeded under --network=none")
	}
}

func TestProbe_SealedSourceReadOnly(t *testing.T) {
	err := os.WriteFile("/snapshot/hostile.txt", []byte("tamper"), 0644)
	if err == nil {
		t.Fatalf("security violation: /snapshot was writable")
	}
}

func TestProbe_WorkDirWritable(t *testing.T) {
	f, err := os.CreateTemp("/work", "test-work-*")
	if err != nil {
		t.Fatalf("expected /work to be writable: %v", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
}
`
		if err := os.WriteFile(filepath.Join(fixtureDir, "fixture_test.go"), []byte(testCode), 0644); err != nil {
			t.Fatal(err)
		}

		runnerCfg := &config.Config{
			SandboxCPUs:             2.0,
			SandboxMemoryBytes:      2147483648,
			SandboxPidsLimit:        256,
			SandboxDiskBytes:        MaxSlotCapacityBytes,
			SandboxTimeoutExecution: 5 * time.Minute,
			SandboxTimeoutCleanup:   15 * time.Second,
		}
		runner := NewRunnerWithConfig(runnerCfg, backend, slotMgr)

		sha := computeDirSHA(fixtureDir)
		snapshot := &Snapshot{
			CommitSHA: sha,
			SourceDir: fixtureDir,
		}

		report, err := runner.RunSnapshot(ctx, snapshot)
		if err != nil {
			t.Fatalf("RunSnapshot failed unexpectedly: %v", err)
		}
		if report.Status != StatusPassed {
			t.Fatalf("expected StatusPassed, got status=%q, reason=%q", report.Status, report.Reason)
		}
		t.Logf("Live snapshot execution PASSED: both build and race-test succeeded")

		// Verify slot cleaned up
		for _, sub := range []string{"work", "tmp", "cache", "snapshot"} {
			entries, err := os.ReadDir(filepath.Join(slotDir, sub))
			if err == nil && len(entries) > 0 {
				t.Fatalf("slot directory %q not cleaned up: %d items remain", sub, len(entries))
			}
		}
		t.Logf("Live slot cleanup verified")
	})
}
