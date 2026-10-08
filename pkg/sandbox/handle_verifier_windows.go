//go:build windows

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandleW = kernel32.NewProc("GetFinalPathNameByHandleW")
)

const volumeNameDOS = 0x0 // FILE_NAME_NORMALIZED

// getFinalPathName queries the Windows kernel for the normalized target path of an open handle.
func getFinalPathName(h syscall.Handle) (string, error) {
	const initialBufSize = 1024
	const maxPathLen = 32768

	buf := make([]uint16, initialBufSize)
	for {
		r, _, err := procGetFinalPathNameByHandleW.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			volumeNameDOS,
		)
		if r == 0 {
			return "", fmt.Errorf("failed to query file handle path: %w", err)
		}
		if r < uintptr(len(buf)) {
			return syscall.UTF16ToString(buf[:r]), nil
		}
		if r > maxPathLen {
			return "", fmt.Errorf("file handle path exceeds maximum allowed length (%d > %d)", r, maxPathLen)
		}
		buf = make([]uint16, r)
	}
}

// verifySnapshotFileHandle verifies on Windows that the open handle points
// strictly within canonicalWorkDir and does not point to restricted paths like .git.
func verifySnapshotFileHandle(f *os.File, canonicalWorkDir string) error {
	finalPath, err := getFinalPathName(syscall.Handle(f.Fd()))
	if err != nil {
		return err
	}

	if strings.HasPrefix(finalPath, `\\?\UNC\`) {
		finalPath = `\\` + strings.TrimPrefix(finalPath, `\\?\UNC\`)
	} else if strings.HasPrefix(finalPath, `\\?\`) {
		finalPath = strings.TrimPrefix(finalPath, `\\?\`)
	}

	cleanFinal := filepath.Clean(finalPath)
	if evaled, err := filepath.EvalSymlinks(cleanFinal); err == nil {
		cleanFinal = evaled
	}

	rel, err := filepath.Rel(canonicalWorkDir, cleanFinal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file handle points outside workspace (%s)", cleanFinal)
	}

	if isGitPath(filepath.ToSlash(rel)) {
		return fmt.Errorf(".git paths are restricted")
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
