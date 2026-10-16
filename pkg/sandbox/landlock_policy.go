package sandbox

// Pure, cross-platform Landlock ABI policy.
//
// This file deliberately imports nothing: it is compiled into every platform,
// including the non-Linux helper build that must still link the shared
// constants. `golang.org/x/sys/unix` is Linux/BSD-only, so the access bits are
// restated here as fixed constants of the Landlock UAPI. These values are
// architecture-independent (they are kernel header constants, not register
// layouts); the names below match the kernel's LANDLOCK_ACCESS_FS_* spellings
// so a reader can diff them against the header.
const (
	accessFSExecute    = 0x1    // LANDLOCK_ACCESS_FS_EXECUTE
	accessFSWriteFile  = 0x2    // LANDLOCK_ACCESS_FS_WRITE_FILE
	accessFSReadFile   = 0x4    // LANDLOCK_ACCESS_FS_READ_FILE
	accessFSReadDir    = 0x8    // LANDLOCK_ACCESS_FS_READ_DIR
	accessFSRemoveDir  = 0x10   // LANDLOCK_ACCESS_FS_REMOVE_DIR
	accessFSRemoveFile = 0x20   // LANDLOCK_ACCESS_FS_REMOVE_FILE
	accessFSMakeChar   = 0x40   // LANDLOCK_ACCESS_FS_MAKE_CHAR
	accessFSMakeDir    = 0x80   // LANDLOCK_ACCESS_FS_MAKE_DIR
	accessFSMakeReg    = 0x100  // LANDLOCK_ACCESS_FS_MAKE_REG
	accessFSMakeSock   = 0x200  // LANDLOCK_ACCESS_FS_MAKE_SOCK
	accessFSMakeFifo   = 0x400  // LANDLOCK_ACCESS_FS_MAKE_FIFO
	accessFSMakeBlock  = 0x800  // LANDLOCK_ACCESS_FS_MAKE_BLOCK
	accessFSMakeSym    = 0x1000 // LANDLOCK_ACCESS_FS_MAKE_SYM
	accessFSRefer      = 0x2000 // LANDLOCK_ACCESS_FS_REFER
	accessFSTruncate   = 0x4000 // LANDLOCK_ACCESS_FS_TRUNCATE
	accessFSIoctlDev   = 0x8000 // LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// MinLandlockABI is the lowest ABI this workload can run under.
//
// LANDLOCK_ACCESS_FS_REFER is denied by default even when it is not listed in
// a ruleset's handled mask, and it cannot be granted at all on ABI v1. The
// `go build`/`go test`/`go mod download` rename-and-replace pattern needs
// REFER on writable subtrees, so an ABI v1 host would produce a sandbox that
// breaks every build. ABI < 2 is therefore reported as rank-unavailable rather
// than silently enforced with a policy that cannot work (T-10-07).
const MinLandlockABI = 2

// landlockAccessFSv1Base is the exact set of filesystem rights ABI v1 knows.
const landlockAccessFSv1Base = accessFSExecute |
	accessFSWriteFile |
	accessFSReadFile |
	accessFSReadDir |
	accessFSRemoveDir |
	accessFSRemoveFile |
	accessFSMakeChar |
	accessFSMakeDir |
	accessFSMakeReg |
	accessFSMakeSock |
	accessFSMakeFifo |
	accessFSMakeBlock |
	accessFSMakeSym

// HandledAccessFS returns the filesystem access mask a ruleset must handle for
// the observed Landlock ABI. Rights unknown to an ABI must never be set in the
// handled mask: the kernel rejects an unknown bit with EINVAL.
//
// The table is monotone by ABI:
//
//	0        — no Landlock: empty mask
//	1        — the 13 v1 base rights (no REFER: it cannot be granted)
//	2        — + REFER
//	3        — + TRUNCATE
//	4        — + IOCTL_DEV
//	5 and 6  — no new filesystem rights, so identical to ABI 4
func HandledAccessFS(abi int) uint64 {
	if abi < 1 {
		return 0
	}
	mask := uint64(landlockAccessFSv1Base)
	if abi >= 2 {
		mask |= accessFSRefer
	}
	if abi >= 3 {
		mask |= accessFSTruncate
	}
	if abi >= 4 {
		mask |= accessFSIoctlDev
	}
	return mask
}

// LandlockRankSupported reports whether an observed ABI can back the landlock
// rank. ABI 0 means the version query failed; ABI 1 cannot grant REFER.
func LandlockRankSupported(abi int) bool {
	return abi >= MinLandlockABI
}

// AllowedReadOnlyAccess derives the rights granted on a read-only root: only
// reads and execution survive. REFER, TRUNCATE and IOCTL_DEV are never granted
// on a root the caller declared read-only.
func AllowedReadOnlyAccess(abi int) uint64 {
	return HandledAccessFS(abi) & uint64(accessFSReadFile|accessFSReadDir|accessFSExecute)
}

// AllowedReadWriteAccess derives the rights granted on a read-write root: the
// full handled mask, so REFER (from ABI 2), TRUNCATE (from ABI 3), EXECUTE and
// the MAKE_*/REMOVE_* rights are all available inside the writable subtree.
func AllowedReadWriteAccess(abi int) uint64 {
	return HandledAccessFS(abi)
}
