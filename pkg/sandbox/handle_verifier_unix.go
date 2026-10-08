//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// verifySnapshotFileHandle verifies on Unix platforms that the open file descriptor points
// strictly within canonicalWorkDir and does not point to restricted paths like .git.
func verifySnapshotFileHandle(f *os.File, canonicalWorkDir string) error {
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

	return nil
}

func isGitPath(slashRel string) bool {
	lower := strings.ToLower(slashRel)
	parts := strings.Split(lower, "/")
	for _, part := range parts {
		cleanPart := strings.TrimRight(part, ". ")
		if cleanPart == ".git" || strings.HasPrefix(cleanPart, ".git:") {
			return true
		}
	}
	return lower == ".git" || strings.HasPrefix(lower, ".git/") ||
		strings.Contains(lower, "/.git/") || strings.HasSuffix(lower, "/.git")
}
