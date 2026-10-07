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
// It is opt-in via PR_REVIEW_SANDBOX_INTEGRATION=1 and requires operator-provisioned
// storage slot and preloaded container image.
// When enabled, it MUST fail on missing prerequisites and never silently skip.
func TestPodmanTracer(t *testing.T) {
	if os.Getenv("PR_REVIEW_SANDBOX_INTEGRATION") != "1" {
		t.Skip("skipping live deployment tracer: set PR_REVIEW_SANDBOX_INTEGRATION=1 to enable")
	}

	// 1. Hard prerequisite checks (must error, never skip)
	podmanPath, err := exec.LookPath("podman")
	if err != nil {
		t.Fatalf("prerequisite missing: podman not found in PATH: %v", err)
	}

	imageDigest := os.Getenv("PR_REVIEW_SANDBOX_IMAGE")
	if imageDigest == "" {
		t.Fatalf("prerequisite missing: PR_REVIEW_SANDBOX_IMAGE must be set to preloaded trusted image digest")
	}

	slotDir := os.Getenv("PR_REVIEW_SLOT_DIR")
	if slotDir == "" {
		t.Fatalf("prerequisite missing: PR_REVIEW_SLOT_DIR must be set to operator-provisioned ext4 mount slot")
	}

	controlDir := os.Getenv("PR_REVIEW_CONTROL_DIR")
	if controlDir == "" {
		controlDir = t.TempDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 2. Initialize SlotManager and Backend
	slotMgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("failed to initialize slot manager: %v", err)
	}

	// Verify production slot capacity <= 3 GiB and mountinfo
	mountID, err := slotMgr.ValidateSlotMount(slotDir)
	if err != nil {
		t.Fatalf("operator slot %q failed validation: %v", slotDir, err)
	}
	t.Logf("Operator slot verified: %q (mount ID %d)", slotDir, mountID)

	podmanCfg := PodmanConfig{
		BinaryPath:   podmanPath,
		ImageDigest:  imageDigest,
		CPUs:         2.0,
		MemoryBytes:  2147483648, // 2 GiB
		PidsLimit:    256,
		TimeoutStage: 5 * time.Minute,
		TimeoutClean: 15 * time.Second,
	}
	backend := NewPodmanBackend(podmanCfg)

	// Live attestation: rootless, cgroup-v2, delegation
	if err := backend.Attest(ctx); err != nil {
		t.Fatalf("podman capability attestation failed on deployed runtime: %v", err)
	}
	t.Logf("Podman capability attestation PASSED")

	// 3. Prepare benign local Go fixture with passing tests
	fixtureDir := t.TempDir()
	goMod := "module tracer.test/fixture\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(fixtureDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	code := `package fixture

func Add(a, b int) int {
	return a + b
}
`
	if err := os.WriteFile(filepath.Join(fixtureDir, "fixture.go"), []byte(code), 0644); err != nil {
		t.Fatal(err)
	}

	testCode := `package fixture

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2, 3) != 5")
	}
}

// Security probe 1: External network must be completely inaccessible
func TestProbe_NetworkIsolation(t *testing.T) {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	_, err := d.Dial("tcp", "1.1.1.1:80")
	if err == nil {
		t.Fatalf("security violation: network connection succeeded under --network=none")
	}
}

// Security probe 2: Source directory in /snapshot must be read-only
func TestProbe_SealedSourceReadOnly(t *testing.T) {
	err := os.WriteFile("/snapshot/hostile.txt", []byte("tamper"), 0644)
	if err == nil {
		t.Fatalf("security violation: /snapshot was writable")
	}
}

// Security probe 3: Writable directories are in the slot
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

	// 4. Run passing snapshot
	t.Logf("Executing passing snapshot...")
	report, err := runner.RunSnapshot(ctx, snapshot)
	if err != nil {
		t.Fatalf("RunSnapshot failed unexpectedly: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed, got status=%q, reason=%q, summary=%q",
			report.Status, report.Reason, report.Summary)
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 execution results (build + race test), got %d", len(report.Results))
	}
	if !report.Results[0].Passed || !report.Results[1].Passed {
		t.Fatalf("both stages must pass: build=%v, test=%v", report.Results[0].Passed, report.Results[1].Passed)
	}
	t.Logf("Passing snapshot verification PASSED")

	// 5. Test failing test case
	failingFixtureDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(failingFixtureDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	failingTest := `package fixture
import "testing"
func TestFail(t *testing.T) {
	t.Fatalf("intentional failure")
}
`
	if err := os.WriteFile(filepath.Join(failingFixtureDir, "fixture_test.go"), []byte(failingTest), 0644); err != nil {
		t.Fatal(err)
	}

	failingSHA := computeDirSHA(failingFixtureDir)
	failingSnapshot := &Snapshot{
		CommitSHA: failingSHA,
		SourceDir: failingFixtureDir,
	}

	t.Logf("Executing failing snapshot...")
	failReport, err := runner.RunSnapshot(ctx, failingSnapshot)
	if err != nil {
		t.Fatalf("RunSnapshot failed unexpectedly: %v", err)
	}
	if failReport.Status != StatusTestFailed {
		t.Fatalf("expected StatusTestFailed for failing test, got: %q", failReport.Status)
	}
	if !strings.Contains(failReport.Summary, "FAILED") {
		t.Fatalf("expected FAILED summary, got: %q", failReport.Summary)
	}
	t.Logf("Failing snapshot verification correctly reported test_failed")

	// 6. Test output bounds
	floodFixtureDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(floodFixtureDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	floodTest := `package fixture
import (
	"fmt"
	"testing"
)
func TestFloodOutput(t *testing.T) {
	for i := 0; i < 50000; i++ {
		fmt.Println("flooding stdout with many lines of test output to exceed 100 KiB")
	}
}
`
	if err := os.WriteFile(filepath.Join(floodFixtureDir, "fixture_test.go"), []byte(floodTest), 0644); err != nil {
		t.Fatal(err)
	}
	floodSnapshot := &Snapshot{
		CommitSHA: computeDirSHA(floodFixtureDir),
		SourceDir: floodFixtureDir,
	}

	t.Logf("Executing output flood snapshot...")
	floodReport, err := runner.RunSnapshot(ctx, floodSnapshot)
	if err != nil {
		t.Fatalf("RunSnapshot failed unexpectedly: %v", err)
	}
	if !floodReport.Truncated {
		t.Fatalf("expected output to be marked truncated")
	}
	if floodReport.DroppedBytes <= 0 {
		t.Fatalf("expected dropped bytes > 0, got %d", floodReport.DroppedBytes)
	}
	t.Logf("Output flood verified: retained output capped, dropped %d bytes", floodReport.DroppedBytes)

	// 7. Verify slot cleaned up after execution
	for _, sub := range []string{"work", "tmp", "cache", "snapshot"} {
		entries, err := os.ReadDir(filepath.Join(slotDir, sub))
		if err == nil && len(entries) > 0 {
			t.Fatalf("slot directory %q not cleaned up: %d items remain", sub, len(entries))
		}
	}
	t.Logf("Slot cleanup verification PASSED")
}
