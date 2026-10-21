//go:build !linux

package sandbox

import "runtime"

// queryLandlockABI reports the typed non-Linux unavailable reason. The
// landlock rank cannot exist off Linux, so this never attempts a syscall.
func queryLandlockABI() (int, error) {
	return 0, newLandlockProbeError(LandlockReasonNotLinux, 0, 0, "GOOS="+runtime.GOOS)
}
