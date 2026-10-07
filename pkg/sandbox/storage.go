package sandbox

import (
	"context"
	"errors"
	"os"
	"time"
)

// MaxSlotCapacityBytes is the hard cap of 3 GiB (3221225472 bytes) from D-07.
const MaxSlotCapacityBytes int64 = 3221225472

const (
	StateLeased      = "leased"
	StateCleaning    = "cleaning"
	StateIdle        = "idle"
	StateQuarantined = "quarantined"
)

var (
	ErrStorageUnavailable = errors.New("sandbox storage slot unavailable")
	ErrSlotBusy           = errors.New("sandbox storage slot is currently leased by another job")
	ErrSlotQuarantined    = errors.New("sandbox storage slot is quarantined due to prior failure")
	ErrInvalidSlotMount   = errors.New("sandbox storage slot mount is invalid or unverified")
	ErrCapacityExceeded   = errors.New("sandbox storage slot exceeds fixed 3 GiB capacity")
	ErrCleanupFailed      = errors.New("sandbox storage slot cleanup failed")
)

// SlotLease represents an exclusive lease over a verified fixed ext4 filesystem slot.
type SlotLease struct {
	SlotDir       string    `json:"slot_dir"`
	JobID         string    `json:"job_id"`
	MountID       int       `json:"mount_id"`
	ControlDir    string    `json:"control_dir"`
	WorkDir       string    `json:"work_dir"`
	TmpDir        string    `json:"tmp_dir"`
	CacheDir      string    `json:"cache_dir"`
	SnapshotDir   string    `json:"snapshot_dir"`
	LockFile      *os.File  `json:"-"`
	StateFilePath string    `json:"state_file_path"`
	AcquiredAt    time.Time `json:"acquired_at"`
	Released      bool      `json:"released"`
}

// SlotState records the persistent state of a slot in the control directory.
type SlotState struct {
	JobID       string    `json:"job_id"`
	MountID     int       `json:"mount_id"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
}

// SlotManager manages exclusive leases over operator-provisioned storage slots.
type SlotManager interface {
	AcquireSlot(ctx context.Context, jobID string) (*SlotLease, error)
	ReleaseSlot(lease *SlotLease) error
	QuarantineSlot(slotDir, reason string) error
	IsQuarantined(slotDir string) bool
	ReconcileSlots(ctx context.Context) error
}
