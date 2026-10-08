//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMountInfoLine(t *testing.T) {
	line := "42 21 8:1 / /var/lib/slots/slot-0 rw,nodev,nosuid,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro"
	entry, err := ParseMountInfoLine(line)
	if err != nil {
		t.Fatalf("unexpected error parsing valid line: %v", err)
	}

	if entry.MountID != 42 {
		t.Errorf("expected MountID 42, got %d", entry.MountID)
	}
	if entry.ParentID != 21 {
		t.Errorf("expected ParentID 21, got %d", entry.ParentID)
	}
	if entry.MountPoint != "/var/lib/slots/slot-0" {
		t.Errorf("expected MountPoint '/var/lib/slots/slot-0', got %q", entry.MountPoint)
	}
	if entry.FSType != "ext4" {
		t.Errorf("expected FSType 'ext4', got %q", entry.FSType)
	}
	if entry.MountSource != "/dev/sda1" {
		t.Errorf("expected MountSource '/dev/sda1', got %q", entry.MountSource)
	}
	opts := strings.Join(entry.MountOptions, ",")
	if !strings.Contains(opts, "nodev") || !strings.Contains(opts, "nosuid") {
		t.Errorf("expected nodev and nosuid in options, got %q", opts)
	}
}

func TestFindMountInfoEntry(t *testing.T) {
	data := `
21 1 0:21 / /sys rw,nosuid,nodev,noexec - sysfs sysfs rw
22 1 0:22 / /proc rw,nosuid,nodev,noexec - proc proc rw
42 1 8:1 / /var/lib/slots/slot-0 rw,nodev,nosuid - ext4 /dev/sda1 rw
`
	entry, err := FindMountInfoEntry(strings.NewReader(data), "/var/lib/slots/slot-0")
	if err != nil {
		t.Fatalf("unexpected error finding entry: %v", err)
	}
	if entry.MountID != 42 {
		t.Errorf("expected MountID 42, got %d", entry.MountID)
	}

	_, err = FindMountInfoEntry(strings.NewReader(data), "/nonexistent")
	if err == nil {
		t.Fatalf("expected error for nonexistent mount, got nil")
	}
}

func TestSlotLockExclusivityAndRelease(t *testing.T) {
	slotDir := t.TempDir()
	controlDir := t.TempDir()

	mgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("failed to create slot manager: %v", err)
	}
	mgr.SkipMountChecks = true // Test locking/lifecycle in userland temp dir

	ctx := context.Background()
	lease1, err := mgr.AcquireSlot(ctx, "job-1")
	if err != nil {
		t.Fatalf("failed to acquire first lease: %v", err)
	}
	if lease1 == nil {
		t.Fatalf("expected non-nil lease1")
	}

	// Second concurrent attempt must fail with ErrSlotBusy
	_, err = mgr.AcquireSlot(ctx, "job-2")
	if err != ErrSlotBusy {
		t.Fatalf("expected ErrSlotBusy on second acquire, got: %v", err)
	}

	// Release first lease
	if err := mgr.ReleaseSlot(lease1); err != nil {
		t.Fatalf("failed to release lease1: %v", err)
	}

	// Now third attempt should succeed
	lease3, err := mgr.AcquireSlot(ctx, "job-3")
	if err != nil {
		t.Fatalf("failed to acquire lease3 after release: %v", err)
	}
	defer mgr.ReleaseSlot(lease3)
}

func TestSlotQuarantinePreventsLease(t *testing.T) {
	slotDir := t.TempDir()
	controlDir := t.TempDir()

	mgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	mgr.SkipMountChecks = true

	if mgr.IsQuarantined(slotDir) {
		t.Fatalf("slot should not be quarantined initially")
	}

	// Quarantine slot
	if err := mgr.QuarantineSlot(slotDir, "test failure"); err != nil {
		t.Fatalf("failed to quarantine: %v", err)
	}

	if !mgr.IsQuarantined(slotDir) {
		t.Fatalf("expected IsQuarantined to return true")
	}

	// Acquire should fail with ErrSlotQuarantined
	_, err = mgr.AcquireSlot(context.Background(), "job-1")
	if err == nil || !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("expected quarantined error, got: %v", err)
	}
}

func TestSlotReconcileOrphanState(t *testing.T) {
	slotDir := t.TempDir()
	controlDir := t.TempDir()

	mgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	mgr.SkipMountChecks = true

	// Simulate orphan state from previous crashed job
	slotName := filepath.Base(slotDir)
	statePath := filepath.Join(controlDir, slotName+".state")
	orphanState := `{"job_id":"orphan-job","mount_id":0,"state":"leased","started_at":"2026-10-07T10:00:00Z"}`
	if err := os.WriteFile(statePath, []byte(orphanState), 0600); err != nil {
		t.Fatal(err)
	}

	// Create leftover files in work dir
	workDir := filepath.Join(slotDir, "work")
	_ = os.MkdirAll(workDir, 0755)
	_ = os.WriteFile(filepath.Join(workDir, "stale.txt"), []byte("stale"), 0644)

	ctx := context.Background()
	if err := mgr.ReconcileSlots(ctx); err != nil {
		t.Fatalf("ReconcileSlots failed: %v", err)
	}

	// Verify stale file was removed
	if _, err := os.Stat(filepath.Join(workDir, "stale.txt")); err == nil {
		t.Fatalf("expected stale file to be removed by ReconcileSlots")
	}

	// Verify new lease can be acquired cleanly
	lease, err := mgr.AcquireSlot(ctx, "fresh-job")
	if err != nil {
		t.Fatalf("expected clean acquire after reconcile, got: %v", err)
	}
	defer mgr.ReleaseSlot(lease)
}

func TestSlotRelease_CleanupFailureQuarantinesSlot(t *testing.T) {
	slotDir := t.TempDir()
	controlDir := t.TempDir()

	mgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	mgr.SkipMountChecks = true

	ctx := context.Background()
	lease, err := mgr.AcquireSlot(ctx, "job-fail-cleanup")
	if err != nil {
		t.Fatalf("failed to acquire lease: %v", err)
	}

	// Simulate escaping/tampered directory path in lease that violates confinement
	lease.WorkDir = "/outside/path"

	err = mgr.ReleaseSlot(lease)
	if err == nil {
		t.Fatalf("expected ReleaseSlot to fail with ErrCleanupFailed, got nil")
	}

	// Verify slot is now quarantined
	if !mgr.IsQuarantined(slotDir) {
		t.Fatalf("expected slot to be marked quarantined after cleanup failure")
	}

	// Subsequent acquire must fail with ErrSlotQuarantined
	_, err = mgr.AcquireSlot(ctx, "next-job")
	if err == nil || !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("expected ErrSlotQuarantined on next job acquire, got: %v", err)
	}
}

