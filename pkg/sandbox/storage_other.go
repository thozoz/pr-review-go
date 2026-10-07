//go:build !linux

package sandbox

import (
	"context"
	"fmt"
)

// LinuxSlotManager is a stub on non-Linux platforms.
type LinuxSlotManager struct {
	SlotDir    string
	ControlDir string
}

func NewLinuxSlotManager(slotDir, controlDir string) (*LinuxSlotManager, error) {
	return nil, fmt.Errorf("%w: fixed slot leasing is only supported on Linux", ErrStorageUnavailable)
}

func (m *LinuxSlotManager) AcquireSlot(ctx context.Context, jobID string) (*SlotLease, error) {
	return nil, fmt.Errorf("%w: fixed slot leasing is only supported on Linux", ErrStorageUnavailable)
}

func (m *LinuxSlotManager) ReleaseSlot(lease *SlotLease) error {
	return nil
}

func (m *LinuxSlotManager) QuarantineSlot(slotDir, reason string) error {
	return nil
}

func (m *LinuxSlotManager) IsQuarantined(slotDir string) bool {
	return false
}

func (m *LinuxSlotManager) ReconcileSlots(ctx context.Context) error {
	return nil
}

func (m *LinuxSlotManager) ValidateSlotMount(dir string) error {
	return fmt.Errorf("%w: fixed slot leasing is only supported on Linux", ErrStorageUnavailable)
}

func (m *LinuxSlotManager) SkipMountChecks() {}

