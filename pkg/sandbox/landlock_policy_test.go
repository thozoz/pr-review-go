package sandbox

import "testing"

// TestHandledAccessFSThroughABI7 pins the exact ABI→mask table. The kernel
// rejects a handled bit it does not know, so every ABI must list precisely the
// rights that ABI introduced (T-10-07, T-11-02): IOCTL_DEV arrives at ABI 5,
// not ABI 4, and ABI 4 == ABI 3 because ABI 4 added only network rights.
func TestHandledAccessFSThroughABI7(t *testing.T) {
	const v1Base = uint64(accessFSExecute | accessFSWriteFile | accessFSReadFile |
		accessFSReadDir | accessFSRemoveDir | accessFSRemoveFile | accessFSMakeChar |
		accessFSMakeDir | accessFSMakeReg | accessFSMakeSock | accessFSMakeFifo |
		accessFSMakeBlock | accessFSMakeSym)
	const allKnown = v1Base | accessFSRefer | accessFSTruncate | accessFSIoctlDev

	if got := HandledAccessFS(0); got != 0 {
		t.Errorf("HandledAccessFS(0) = %#x, want 0", got)
	}
	if got := HandledAccessFS(-1); got != 0 {
		t.Errorf("HandledAccessFS(-1) = %#x, want 0", got)
	}
	if got := HandledAccessFS(1); got != v1Base {
		t.Errorf("HandledAccessFS(1) = %#x, want %#x", got, v1Base)
	}
	if got := HandledAccessFS(2); got != v1Base|accessFSRefer {
		t.Errorf("HandledAccessFS(2) = %#x, want %#x", got, v1Base|accessFSRefer)
	}
	want3 := v1Base | accessFSRefer | accessFSTruncate
	if got := HandledAccessFS(3); got != want3 {
		t.Errorf("HandledAccessFS(3) = %#x, want %#x", got, want3)
	}
	// ABI 4 (Linux 6.7) added only ACCESS_NET_BIND_TCP/CONNECT_TCP: the
	// filesystem mask must be unchanged.
	if got := HandledAccessFS(4); got != want3 {
		t.Errorf("HandledAccessFS(4) = %#x, want HandledAccessFS(3) = %#x (ABI 4 adds no fs rights)", got, want3)
	}
	want5 := want3 | accessFSIoctlDev
	if got := HandledAccessFS(5); got != want5 {
		t.Errorf("HandledAccessFS(5) = %#x, want %#x (ABI 5 adds IOCTL_DEV)", got, want5)
	}
	for _, abi := range []int{6, 7} {
		if got := HandledAccessFS(abi); got != want5 {
			t.Errorf("HandledAccessFS(%d) = %#x, want HandledAccessFS(5) = %#x (ABI %d adds no fs rights)", abi, got, want5, abi)
		}
	}
	for _, abi := range []int{8, 9} {
		if got := HandledAccessFS(abi); got != want5 {
			t.Errorf("HandledAccessFS(%d) = %#x, want saturation at HandledAccessFS(7) = %#x", abi, got, want5)
		}
	}

	if HandledAccessFS(4) != HandledAccessFS(3) {
		t.Error("HandledAccessFS(4) must equal HandledAccessFS(3)")
	}
	if HandledAccessFS(5)&accessFSIoctlDev == 0 {
		t.Error("HandledAccessFS(5) must contain IOCTL_DEV")
	}
	if HandledAccessFS(5) != HandledAccessFS(7) {
		t.Error("HandledAccessFS(5) must equal HandledAccessFS(7)")
	}
	if MaxKnownLandlockABI != 7 {
		t.Errorf("MaxKnownLandlockABI = %d, want 7", MaxKnownLandlockABI)
	}

	// Mono: each ABI is a superset of the previous one, and every mask is a
	// subset of the union of the known bits.
	prev := uint64(0)
	for abi := 0; abi <= 9; abi++ {
		mask := HandledAccessFS(abi)
		if prev&^mask != 0 {
			t.Errorf("HandledAccessFS(%d) = %#x drops bits from HandledAccessFS(%d) = %#x", abi, mask, abi-1, prev)
		}
		if mask&^allKnown != 0 {
			t.Errorf("HandledAccessFS(%d) = %#x contains unknown bits %#x", abi, mask, mask&^allKnown)
		}
		prev = mask
	}

	if LandlockRankSupported(0) || LandlockRankSupported(1) {
		t.Error("ABI 0 and ABI 1 must be unsupported: REFER cannot be granted on v1")
	}
	for abi := 2; abi <= 9; abi++ {
		if !LandlockRankSupported(abi) {
			t.Errorf("ABI %d must be supported", abi)
		}
	}
	if MinLandlockABI != 2 {
		t.Errorf("MinLandlockABI = %d, want 2", MinLandlockABI)
	}
}

// TestAllowedAccessPerABI pins the derived per-root grants: read-only roots
// never receive REFER/TRUNCATE/IOCTL_DEV, and read-write roots receive the
// whole handled mask so the go build/test rename pattern works.
func TestAllowedAccessPerABI(t *testing.T) {
	const readOnly = uint64(accessFSReadFile | accessFSReadDir | accessFSExecute)
	const neverReadOnly = uint64(accessFSRefer | accessFSTruncate | accessFSIoctlDev)

	for abi := 0; abi <= 9; abi++ {
		handled := HandledAccessFS(abi)
		ro := AllowedReadOnlyAccess(abi)
		rw := AllowedReadWriteAccess(abi)

		if ro&^handled != 0 {
			t.Errorf("AllowedReadOnlyAccess(%d) = %#x is not a subset of handled %#x", abi, ro, handled)
		}
		if rw != handled {
			t.Errorf("AllowedReadWriteAccess(%d) = %#x, want the full handled mask %#x", abi, rw, handled)
		}
		if ro&neverReadOnly != 0 {
			t.Errorf("AllowedReadOnlyAccess(%d) = %#x grants a write-class right", abi, ro)
		}
		if handled != 0 {
			if ro&readOnly != readOnly {
				t.Errorf("AllowedReadOnlyAccess(%d) = %#x, want at least %#x", abi, ro, readOnly)
			}
		}
	}

	// REFER is the rename/replace right the build needs: granted on rw roots
	// from ABI 2, never on ro roots.
	for abi := 2; abi <= 9; abi++ {
		if AllowedReadWriteAccess(abi)&accessFSRefer == 0 {
			t.Errorf("AllowedReadWriteAccess(%d) must grant REFER", abi)
		}
		if AllowedReadOnlyAccess(abi)&accessFSRefer != 0 {
			t.Errorf("AllowedReadOnlyAccess(%d) must not grant REFER", abi)
		}
	}

	// IOCTL_DEV is granted on rw roots only from ABI 5.
	if AllowedReadWriteAccess(4)&accessFSIoctlDev != 0 {
		t.Error("AllowedReadWriteAccess(4) must not grant IOCTL_DEV")
	}
	for abi := 5; abi <= 9; abi++ {
		if AllowedReadWriteAccess(abi)&accessFSIoctlDev == 0 {
			t.Errorf("AllowedReadWriteAccess(%d) must grant IOCTL_DEV", abi)
		}
	}
}
