//go:build unix

package assistant

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// verifyFileHandle verifies on Unix platforms that the open file descriptor points
// within canonicalWorkDir and does not access restricted paths.
func verifyFileHandle(f *os.File, canonicalWorkDir string) error {
	// On Linux (and platforms with /proc/self/fd), verify target via procfs
	procPath := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	target, err := os.Readlink(procPath)
	if err == nil {
		cleanTarget := filepath.Clean(target)
		if evaled, err := filepath.EvalSymlinks(cleanTarget); err == nil {
			cleanTarget = evaled
		}

		rel, err := filepath.Rel(canonicalWorkDir, cleanTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("file handle points outside workspace (%s)", cleanTarget)
		}

		if isGitPath(filepath.ToSlash(rel)) {
			return fmt.Errorf(".git paths are restricted")
		}
		return nil
	}

	// On systems without /proc (or if /proc is not mounted), root confinement from
	// os.Root / openat already bound the resolution to the workspace root.
	return nil
}
