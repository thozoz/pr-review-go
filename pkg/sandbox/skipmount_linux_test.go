//go:build linux

package sandbox

import (
	"path/filepath"
	"testing"
)

// Unprivileged LXC / Proxmox operators opt out of mount-table attestation
// via SANDBOX_SKIP_MOUNT_CHECKS=1. The constructor must honor it so
// SlotManagerForConfig and direct users get a skipping manager.
func TestNewLinuxSlotManager_SkipMountChecksEnv(t *testing.T) {
	slotDir := filepath.Join(t.TempDir(), "slot-01")
	controlDir := filepath.Join(t.TempDir(), "control")

	t.Setenv("SANDBOX_SKIP_MOUNT_CHECKS", "1")
	mgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		t.Fatalf("NewLinuxSlotManager failed: %v", err)
	}
	if !mgr.SkipMountChecks {
		t.Error("expected SkipMountChecks=true with SANDBOX_SKIP_MOUNT_CHECKS=1")
	}

	controlDir2 := filepath.Join(t.TempDir(), "control2")
	t.Setenv("SANDBOX_SKIP_MOUNT_CHECKS", "true")
	mgr2, err := NewLinuxSlotManager(slotDir, controlDir2)
	if err != nil {
		t.Fatalf("NewLinuxSlotManager failed: %v", err)
	}
	if !mgr2.SkipMountChecks {
		t.Error("expected SkipMountChecks=true with SANDBOX_SKIP_MOUNT_CHECKS=true")
	}

	controlDir3 := filepath.Join(t.TempDir(), "control3")
	t.Setenv("SANDBOX_SKIP_MOUNT_CHECKS", "")
	mgr3, err := NewLinuxSlotManager(slotDir, controlDir3)
	if err != nil {
		t.Fatalf("NewLinuxSlotManager failed: %v", err)
	}
	if mgr3.SkipMountChecks {
		t.Error("expected SkipMountChecks=false without SANDBOX_SKIP_MOUNT_CHECKS")
	}
}
