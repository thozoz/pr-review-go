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

// MaxKnownLandlockABI is the highest ABI this table has been validated
// against. HandledAccessFS stays total and saturating for any higher ABI, and
// nothing may be gated on v7: v7 adds only the RESTRICT_SELF_LOG_* audit
// flags (no filesystem rights) and its exact release is inconsistently
// reported, so it can never become a feature requirement (T-11-02).
const MaxKnownLandlockABI = 7

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
// The table is monotone by ABI and follows the kernel UAPI exactly:
//
//	0        — no Landlock: empty mask
//	1        — the 13 v1 base rights (no REFER: it cannot be granted)
//	2        — + REFER
//	3        — + TRUNCATE
//	4        — no new filesystem rights, so identical to ABI 3 (Linux 6.7
//	           added only the network rights ACCESS_NET_BIND_TCP/CONNECT_TCP,
//	           which are out of scope here)
//	5        — + IOCTL_DEV (Linux 6.10)
//	6 and 7  — no new filesystem rights, so identical to ABI 5 (v6 adds
//	           SCOPE_*; v7 adds only RESTRICT_SELF_LOG_* audit flags)
//
// LANDLOCK_ACCESS_FS_IOCTL_DEV is "available since the fifth version of the
// Landlock ABI" (/usr/include/linux/landlock.h). Gating it at ABI 4 — as this
// table previously did — sets a handled bit an ABI-4 kernel does not know;
// landlock_create_ruleset then returns EINVAL, the helper exits 71, and the
// rank is wrongly reported unavailable. ABIs above MaxKnownLandlockABI
// saturate at the known mask rather than guessing at new bits.
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
	if abi >= 5 {
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
// full handled mask, so REFER (from ABI 2), TRUNCATE (from ABI 3), IOCTL_DEV
// (from ABI 5), EXECUTE and the MAKE_*/REMOVE_* rights are all available
// inside the writable subtree.
func AllowedReadWriteAccess(abi int) uint64 {
	return HandledAccessFS(abi)
}

// LandlockProbeReason is the typed, fixed classification of why the Landlock
// rank could not be attested. It is deliberately import-free so every platform
// — including the non-Linux helper build — can interpret the same table, and
// so the four distinct kernel causes (ENOSYS, EOPNOTSUPP, EPERM, EINVAL) stay
// separately actionable instead of collapsing into "unknown" (T-11-03).
type LandlockProbeReason string

const (
	// LandlockReasonNone means the probe succeeded; no unavailability.
	LandlockReasonNone LandlockProbeReason = ""
	// LandlockReasonUnsupported — ENOSYS: the kernel has no Landlock at all
	// (predates 5.13 or CONFIG_SECURITY_LANDLOCK is not set).
	LandlockReasonUnsupported LandlockProbeReason = "kernel_unsupported"
	// LandlockReasonDisabled — EOPNOTSUPP: Landlock is compiled in but
	// disabled at boot (the kernel's LSM list omits landlock).
	LandlockReasonDisabled LandlockProbeReason = "disabled_at_boot"
	// LandlockReasonBlocked — EPERM: seccomp or a capability restriction
	// blocked the syscall.
	LandlockReasonBlocked LandlockProbeReason = "blocked"
	// LandlockReasonInvalid — EINVAL or any other create_ruleset failure.
	LandlockReasonInvalid LandlockProbeReason = "invalid_request"
	// LandlockReasonTooOld — the kernel provides an ABI below MinLandlockABI.
	LandlockReasonTooOld LandlockProbeReason = "abi_too_old"
	// LandlockReasonNotLinux — the server is not running on Linux.
	LandlockReasonNotLinux LandlockProbeReason = "not_linux"
	// LandlockReasonUnknown — an errno outside the classified set.
	LandlockReasonUnknown LandlockProbeReason = "unknown_errno"
)

// Linux asm-generic errno values classified by this file. amd64 and arm64
// share this numbering; the linux-only query converts unix.Errno to an int
// before calling LandlockUnavailableReason, which is what keeps this file
// portable and unit-testable without a Linux build tag.
const (
	errnoEPERM      = 1
	errnoEINVAL     = 22
	errnoENOSYS     = 38
	errnoEOPNOTSUPP = 95
)

// LandlockUnavailableReason maps a raw errno from the Landlock version query
// onto its typed reason. Every distinct cause has a different operator
// remediation, so they must never be conflated.
func LandlockUnavailableReason(errno int) LandlockProbeReason {
	switch errno {
	case 0:
		return LandlockReasonNone
	case errnoENOSYS:
		return LandlockReasonUnsupported
	case errnoEOPNOTSUPP:
		return LandlockReasonDisabled
	case errnoEPERM:
		return LandlockReasonBlocked
	case errnoEINVAL:
		return LandlockReasonInvalid
	default:
		return LandlockReasonUnknown
	}
}

// LandlockABIVersionReason reports whether an observed ABI can back this
// workload. ABI < MinLandlockABI (2) is reported as too old: REFER cannot be
// granted below ABI 2, so a sandbox at that ABI would break every
// rename-based build (T-10-07).
func LandlockABIVersionReason(abi int) (LandlockProbeReason, bool) {
	if abi < MinLandlockABI {
		return LandlockReasonTooOld, false
	}
	return LandlockReasonNone, true
}

// LandlockProbeReasonMessage returns fixed, operator-actionable text for one
// probe reason. Every string is server-authored; `detail` is the only caller
// input and must never carry raw target output.
func LandlockProbeReasonMessage(reason LandlockProbeReason, detail string) string {
	var msg string
	switch reason {
	case LandlockReasonUnsupported:
		msg = "landlock unavailable: the kernel does not provide the Landlock syscall (ENOSYS) — " +
			"the kernel predates 5.13 or CONFIG_SECURITY_LANDLOCK is not set; the landlock rank is unavailable"
	case LandlockReasonDisabled:
		msg = "landlock unavailable: Landlock is built into the kernel but disabled at boot (EOPNOTSUPP) — " +
			"the kernel's LSM list does not include landlock; to enable it, add landlock to the `lsm=` list on the " +
			"HOST kernel command line and reboot the host (on Proxmox: /etc/kernel/cmdline plus " +
			"proxmox-boot-tool refresh); this cannot be changed from inside the container; the landlock rank is unavailable"
	case LandlockReasonBlocked:
		msg = "landlock unavailable: the Landlock syscall was blocked (EPERM) — " +
			"the container's seccomp profile or a capability restriction refused it; this is a misconfiguration, " +
			"not an absent kernel feature; the landlock rank is unavailable"
	case LandlockReasonTooOld:
		msg = "landlock unavailable: the kernel provides a Landlock ABI below the minimum — " +
			"LANDLOCK_ACCESS_FS_REFER cannot be granted below ABI 2, so a sandbox at this ABI would break every " +
			"rename-based build; the landlock rank is unavailable for this workload"
	case LandlockReasonInvalid:
		msg = "landlock unavailable: the Landlock version query failed with an unexpected error — " +
			"the request was rejected as invalid; the landlock rank is unavailable"
	case LandlockReasonNotLinux:
		msg = "landlock unavailable: the server is not running on Linux — " +
			"the landlock rank requires a Linux kernel; the landlock rank is unavailable"
	case LandlockReasonUnknown:
		msg = "landlock unavailable: the Landlock version query failed with an unclassified error; " +
			"the landlock rank is unavailable"
	default:
		msg = "landlock unavailable: the probe reported an unclassified reason; the landlock rank is unavailable"
	}
	if detail == "" {
		return msg
	}
	return msg + " (" + detail + ")"
}
