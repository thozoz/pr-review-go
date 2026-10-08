//go:build !windows && !unix

package sandbox

import "os"

// verifySnapshotFileHandle is a fallback for non-Windows and non-Unix platforms where
// handle-to-path resolution is unavailable. On these platforms, containment relies
// on os.Root directory boundary checks.
func verifySnapshotFileHandle(f *os.File, canonicalWorkDir string) error {
	return nil
}
