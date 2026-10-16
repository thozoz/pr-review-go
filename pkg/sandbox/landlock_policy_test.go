package sandbox

import "testing"

// TestHandledAccessFSABITable pins the exact ABI→mask table. The kernel
// rejects a handled bit it does not know, so every ABI must list precisely the
// rights that ABI introduced (T-10-07).
func TestHandledAccessFSABITable(t *testing.T) {
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
	if got := HandledAccessFS(3); got != v1Base|accessFSRefer|accessFSTruncate {
		t.Errorf("HandledAccessFS(3) = %#x, want %#x", got, v1Base|accessFSRefer|accessFSTruncate)
	}
	want4 := v1Base | accessFSRefer | accessFSTruncate | accessFSIoctlDev
	if got := HandledAccessFS(4); got != want4 {
		t.Errorf("HandledAccessFS(4) = %#x, want %#x", got, want4)
	}
	if got := HandledAccessFS(5); got != want4 {
		t.Errorf("HandledAccessFS(5) = %#x, want HandledAccessFS(4) = %#x (ABI 5 adds no fs rights)", got, want4)
	}
	if got := HandledAccessFS(6); got != want4 {
		t.Errorf("HandledAccessFS(6) = %#x, want HandledAccessFS(4) = %#x (ABI 6 adds no fs rights)", got, want4)
	}

	// Mono: each ABI is a superset of the previous one, and every mask is a
	// subset of the union of the known bits.
	prev := uint64(0)
	for abi := 0; abi <= 6; abi++ {
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
	for abi := 2; abi <= 6; abi++ {
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

	for abi := 0; abi <= 6; abi++ {
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
	for abi := 2; abi <= 6; abi++ {
		if AllowedReadWriteAccess(abi)&accessFSRefer == 0 {
			t.Errorf("AllowedReadWriteAccess(%d) must grant REFER", abi)
		}
		if AllowedReadOnlyAccess(abi)&accessFSRefer != 0 {
			t.Errorf("AllowedReadOnlyAccess(%d) must not grant REFER", abi)
		}
	}
}
