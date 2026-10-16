//go:build linux

package sandbox

import (
	"golang.org/x/sys/unix"
)

// queryLandlockABI performs the read-only LANDLOCK_CREATE_RULESET_VERSION
// query and returns the running kernel's Landlock ABI.
//
// Only the ..._VERSION cookie is ever passed from the server. This file must
// never grow a ruleset-creation or domain-application call: the long-lived,
// multi-threaded server may ask what ABI exists, but applying a domain to
// itself is per-thread and irreversible (T-11-01). TestServerABIProbeIsQueryOnly
// enforces this.
func queryLandlockABI() (int, error) {
	// unix.Syscall (not RawSyscall) so the runtime can still handle signals.
	r1, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0, 0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION),
	)
	if errno != 0 {
		reason := LandlockUnavailableReason(int(errno))
		return 0, newLandlockProbeError(reason, int(errno), 0, "")
	}
	if r1 == ^uintptr(0) {
		return 0, newLandlockProbeError(LandlockReasonInvalid, errnoEINVAL, 0, "")
	}
	abi := int(r1)
	if reason, ok := LandlockABIVersionReason(abi); !ok {
		return abi, newLandlockProbeError(reason, 0, abi, "")
	}
	return abi, nil
}
