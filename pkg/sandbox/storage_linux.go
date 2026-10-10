//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LinuxSlotManager implements SlotManager on Linux systems with verified ext4 mounts.
type LinuxSlotManager struct {
	SlotDir          string
	ControlDir       string
	SkipMountChecks  bool // Only for non-privileged unit test harness
}

// MountInfoEntry represents a parsed record from /proc/self/mountinfo.
type MountInfoEntry struct {
	MountID      int
	ParentID     int
	MajorMinor   string
	Root         string
	MountPoint   string
	MountOptions []string
	FSType       string
	MountSource  string
	SuperOptions []string
}

// NewLinuxSlotManager creates a manager for a single operator-provisioned slot.
func NewLinuxSlotManager(slotDir, controlDir string) (*LinuxSlotManager, error) {
	if slotDir == "" {
		return nil, fmt.Errorf("%w: slot directory cannot be empty", ErrStorageUnavailable)
	}
	if controlDir == "" {
		return nil, fmt.Errorf("%w: control directory cannot be empty", ErrStorageUnavailable)
	}

	cleanSlot, err := filepath.Abs(slotDir)
	if err != nil {
		return nil, fmt.Errorf("invalid slot dir %q: %w", slotDir, err)
	}
	cleanControl, err := filepath.Abs(controlDir)
	if err != nil {
		return nil, fmt.Errorf("invalid control dir %q: %w", controlDir, err)
	}

	if err := os.MkdirAll(cleanControl, 0700); err != nil {
		return nil, fmt.Errorf("failed to create control dir: %w", err)
	}

	mgr := &LinuxSlotManager{
		SlotDir:    cleanSlot,
		ControlDir: cleanControl,
	}
	// Unprivileged LXC / Proxmox environments cannot satisfy mount-table
	// attestation (no loop-device passthrough); the operator opts out
	// explicitly via SANDBOX_SKIP_MOUNT_CHECKS=1. Same check is honored in
	// AcquireSlot so managers built before the env change are covered too.
	if os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "1" || os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "true" {
		mgr.SkipMountChecks = true
	}

	return mgr, nil
}

// ParseMountInfoLine parses a single line of /proc/self/mountinfo.
func ParseMountInfoLine(line string) (*MountInfoEntry, error) {
	fields := strings.Fields(line)
	if len(fields) < 7 {
		return nil, fmt.Errorf("mountinfo line has too few fields (%d)", len(fields))
	}

	mountID, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil, fmt.Errorf("invalid mount ID %q: %w", fields[0], err)
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("invalid parent ID %q: %w", fields[1], err)
	}

	// Find the separator "-"
	sepIdx := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 || len(fields) < sepIdx+3 {
		return nil, fmt.Errorf("mountinfo line missing post-separator filesystem fields")
	}

	entry := &MountInfoEntry{
		MountID:      mountID,
		ParentID:     parentID,
		MajorMinor:   fields[2],
		Root:         fields[3],
		MountPoint:   fields[4],
		MountOptions: strings.Split(fields[5], ","),
		FSType:       fields[sepIdx+1],
		MountSource:  fields[sepIdx+2],
	}
	if len(fields) > sepIdx+3 {
		entry.SuperOptions = strings.Split(fields[sepIdx+3], ",")
	}

	return entry, nil
}

// FindMountInfoEntry finds the mount entry matching targetPath in r.
func FindMountInfoEntry(r io.Reader, targetPath string) (*MountInfoEntry, error) {
	scanner := bufio.NewScanner(r)
	cleanTarget := filepath.Clean(targetPath)

	var matched *MountInfoEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entry, err := ParseMountInfoLine(line)
		if err != nil {
			continue
		}
		if filepath.Clean(entry.MountPoint) == cleanTarget {
			matched = entry
			// Do not break early; later mounts overlay earlier ones
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed reading mountinfo: %w", err)
	}
	if matched == nil {
		return nil, fmt.Errorf("%w: no mount found at %q", ErrInvalidSlotMount, targetPath)
	}
	return matched, nil
}

// ValidateSlotMount verifies mountinfo, filesystem magic, capacity and parent ownership.
func (m *LinuxSlotManager) ValidateSlotMount(slotDir string) (int, error) {
	// 1. Verify parent directory ownership: parent must be owned by root (UID 0)
	parentDir := filepath.Dir(slotDir)
	parentFi, err := os.Stat(parentDir)
	if err != nil {
		return 0, fmt.Errorf("%w: failed to stat parent %q: %v", ErrInvalidSlotMount, parentDir, err)
	}
	parentStat, ok := parentFi.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != 0 {
		return 0, fmt.Errorf("%w: slot parent directory %q must be owned by root (uid 0)", ErrInvalidSlotMount, parentDir)
	}

	// 2. Read /proc/self/mountinfo
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return 0, fmt.Errorf("%w: failed to open /proc/self/mountinfo: %v", ErrInvalidSlotMount, err)
	}
	defer f.Close()

	entry, err := FindMountInfoEntry(f, slotDir)
	if err != nil {
		return 0, err
	}

	// Verify filesystem type is ext4
	if entry.FSType != "ext4" {
		return 0, fmt.Errorf("%w: filesystem type is %q (expected ext4)", ErrInvalidSlotMount, entry.FSType)
	}

	// Verify nodev and nosuid mount options
	hasNodev := false
	hasNosuid := false
	for _, opt := range entry.MountOptions {
		if opt == "nodev" {
			hasNodev = true
		}
		if opt == "nosuid" {
			hasNosuid = true
		}
	}
	if !hasNodev || !hasNosuid {
		return 0, fmt.Errorf("%w: mount must have nodev and nosuid options (options: %v)", ErrInvalidSlotMount, entry.MountOptions)
	}

	// 3. Statfs verification (magic and capacity)
	var statfs syscall.Statfs_t
	if err := syscall.Statfs(slotDir, &statfs); err != nil {
		return 0, fmt.Errorf("%w: statfs failed on %q: %v", ErrInvalidSlotMount, slotDir, err)
	}

	// ext4 magic: 0xef53
	const ext4Magic = 0xef53
	if statfs.Type != ext4Magic {
		return 0, fmt.Errorf("%w: filesystem magic is 0x%x (expected 0xef53 ext4)", ErrInvalidSlotMount, statfs.Type)
	}

	// Total capacity check: Blocks * Bsize <= MaxSlotCapacityBytes
	totalBytes := int64(statfs.Blocks) * int64(statfs.Bsize)
	if totalBytes > MaxSlotCapacityBytes {
		return 0, fmt.Errorf("%w: total capacity %d bytes exceeds fixed %d bytes (3 GiB)", ErrCapacityExceeded, totalBytes, MaxSlotCapacityBytes)
	}

	return entry.MountID, nil
}

// IsQuarantined checks if the slot has been marked quarantined.
func (m *LinuxSlotManager) IsQuarantined(slotDir string) bool {
	slotName := filepath.Base(slotDir)
	quarantineFile := filepath.Join(m.ControlDir, slotName+".quarantine")
	_, err := os.Stat(quarantineFile)
	return err == nil
}

// QuarantineSlot writes a durable quarantine record and updates slot state.
func (m *LinuxSlotManager) QuarantineSlot(slotDir, reason string) error {
	slotName := filepath.Base(slotDir)
	quarantineFile := filepath.Join(m.ControlDir, slotName+".quarantine")
	_ = os.WriteFile(quarantineFile, []byte(fmt.Sprintf("%s\nTimestamp: %s\n", reason, time.Now().UTC().Format(time.RFC3339))), 0600)

	stateFile := filepath.Join(m.ControlDir, slotName+".state")
	state := SlotState{
		State:            StateQuarantined,
		StartedAt:        time.Now().UTC(),
		QuarantineReason: reason,
	}
	data, _ := json.Marshal(state)
	_ = os.WriteFile(stateFile, data, 0600)
	return nil
}

// AcquireSlot claims an exclusive lease on the slot.
func (m *LinuxSlotManager) AcquireSlot(ctx context.Context, jobID string) (*SlotLease, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: jobID is empty", ErrStorageUnavailable)
	}

	slotDir := m.SlotDir
	slotName := filepath.Base(slotDir)

	if m.IsQuarantined(slotDir) {
		return nil, fmt.Errorf("%w: slot %q is quarantined", ErrSlotQuarantined, slotDir)
	}

	mountID := 0
	skipChecks := m.SkipMountChecks ||
		os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "1" ||
		os.Getenv("SANDBOX_SKIP_MOUNT_CHECKS") == "true"
	if !skipChecks {
		var err error
		mountID, err = m.ValidateSlotMount(slotDir)
		if err != nil {
			return nil, err
		}
	}

	// Acquire exclusive OS lock outside child mounts
	lockPath := filepath.Join(m.ControlDir, slotName+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to open slot lock file: %v", ErrStorageUnavailable, err)
	}

	// Try non-blocking flock
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrSlotBusy
		}
		return nil, fmt.Errorf("%w: failed to lock slot: %v", ErrStorageUnavailable, err)
	}

	// Write durable leased state
	statePath := filepath.Join(m.ControlDir, slotName+".state")
	state := SlotState{
		JobID:     jobID,
		MountID:   mountID,
		State:     StateLeased,
		StartedAt: time.Now().UTC(),
	}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		return nil, fmt.Errorf("failed to marshal state: %w", err)
	}
	if err := os.WriteFile(statePath, stateBytes, 0600); err != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		return nil, fmt.Errorf("failed to write state file: %w", err)
	}

	// Prepare clean subdirectories
	workDir := filepath.Join(slotDir, "work")
	tmpDir := filepath.Join(slotDir, "tmp")
	cacheDir := filepath.Join(slotDir, "cache")
	snapshotDir := filepath.Join(slotDir, "snapshot")

	for _, d := range []string{workDir, tmpDir, cacheDir, snapshotDir} {
		_ = os.RemoveAll(d)
		if err := os.MkdirAll(d, 0755); err != nil {
			_ = m.QuarantineSlot(slotDir, fmt.Sprintf("failed to initialize directory %q: %v", d, err))
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, fmt.Errorf("failed to create directory %q: %w", d, err)
		}
	}

	return &SlotLease{
		SlotDir:       slotDir,
		JobID:         jobID,
		MountID:       mountID,
		ControlDir:    m.ControlDir,
		WorkDir:       workDir,
		TmpDir:        tmpDir,
		CacheDir:      cacheDir,
		SnapshotDir:   snapshotDir,
		LockFile:      lockFile,
		StateFilePath: statePath,
		AcquiredAt:    time.Now().UTC(),
		Released:      false,
	}, nil
}

// ReleaseSlot cleans up slot contents safely and releases the lock.
func (m *LinuxSlotManager) ReleaseSlot(lease *SlotLease) error {
	if lease == nil || lease.Released {
		return nil
	}

	slotDir := lease.SlotDir

	// Update state to cleaning
	state := SlotState{
		JobID:     lease.JobID,
		MountID:   lease.MountID,
		State:     StateCleaning,
		StartedAt: lease.AcquiredAt,
	}
	if data, err := json.Marshal(state); err == nil {
		_ = os.WriteFile(lease.StateFilePath, data, 0600)
	}

	// Safely remove contents within slotDir
	var cleanErr error
	for _, d := range []string{lease.WorkDir, lease.TmpDir, lease.CacheDir, lease.SnapshotDir} {
		// Confinement check: directory must be inside slotDir
		rel, err := filepath.Rel(slotDir, d)
		if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
			cleanErr = fmt.Errorf("directory %q is outside slot %q", d, slotDir)
			break
		}
		if err := os.RemoveAll(d); err != nil {
			cleanErr = fmt.Errorf("failed to remove %q: %w", d, err)
			break
		}
	}

	if cleanErr != nil {
		_ = m.QuarantineSlot(slotDir, fmt.Sprintf("cleanup failure on release: %v", cleanErr))
		if lease.LockFile != nil {
			_ = syscall.Flock(int(lease.LockFile.Fd()), syscall.LOCK_UN)
			_ = lease.LockFile.Close()
		}
		lease.Released = true
		return fmt.Errorf("%w: %v", ErrCleanupFailed, cleanErr)
	}

	// Mark state idle
	state.State = StateIdle
	if data, err := json.Marshal(state); err == nil {
		_ = os.WriteFile(lease.StateFilePath, data, 0600)
	}

	if lease.LockFile != nil {
		_ = syscall.Flock(int(lease.LockFile.Fd()), syscall.LOCK_UN)
		_ = lease.LockFile.Close()
	}
	lease.Released = true
	return nil
}

// ReconcileSlots inspects persistent slot state files and reconciles orphaned leases.
func (m *LinuxSlotManager) ReconcileSlots(ctx context.Context) error {
	entries, err := os.ReadDir(m.ControlDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".state") {
			statePath := filepath.Join(m.ControlDir, entry.Name())
			data, err := os.ReadFile(statePath)
			if err != nil {
				continue
			}
			var state SlotState
			if err := json.Unmarshal(data, &state); err != nil {
				continue
			}
			if state.State == StateLeased || state.State == StateCleaning {
				slotName := strings.TrimSuffix(entry.Name(), ".state")
				slotPath := m.SlotDir
				if filepath.Base(slotPath) == slotName {
					// Clean slot subdirectories
					workDir := filepath.Join(slotPath, "work")
					tmpDir := filepath.Join(slotPath, "tmp")
					cacheDir := filepath.Join(slotPath, "cache")
					snapDir := filepath.Join(slotPath, "snapshot")
					cleanFailed := false
					for _, d := range []string{workDir, tmpDir, cacheDir, snapDir} {
						if err := os.RemoveAll(d); err != nil {
							cleanFailed = true
							break
						}
					}
					if cleanFailed {
						_ = m.QuarantineSlot(slotPath, "reconciliation cleanup failed")
					} else {
						state.State = StateIdle
						if d, err := json.Marshal(state); err == nil {
							_ = os.WriteFile(statePath, d, 0600)
						}
					}
				}
			}
		}
	}
	return nil
}

