//go:build !windows && !unix

package reviewer

import "os"

// verifyFileHandle is a fallback for non-Windows and non-Unix platforms where
// handle-to-path resolution is unavailable. On these platforms, containment relies
// on os.Root directory boundary checks.
func verifyFileHandle(f *os.File, canonicalWorkDir string) error {
	return nil
}
