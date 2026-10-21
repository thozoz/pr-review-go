//go:build !windows && !unix

package assistant

import "os"

// VerifyFileHandleInRoot is the exported seam over verifyFileHandle for
// platforms without handle-to-path resolution. It mirrors the unix/windows
// signature so callers compile everywhere (08-REVIEW CR-02).
func VerifyFileHandleInRoot(f *os.File, canonicalWorkDir string) error {
	return verifyFileHandle(f, canonicalWorkDir)
}

// verifyFileHandle is a fallback for non-Windows and non-Unix platforms where
// handle-to-path resolution is unavailable. On these platforms, containment relies
// on os.Root directory boundary checks.
func verifyFileHandle(f *os.File, canonicalWorkDir string) error {
	return nil
}
