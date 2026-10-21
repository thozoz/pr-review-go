//go:build windows

package assistant

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

const (
	volumeNameDOS = 0x0 // FILE_NAME_NORMALIZED
)

// getFinalPathName queries the Windows kernel for the normalized target path of an open handle,
// supporting dynamic buffer resizing when paths exceed initial buffer capacity up to maxPathLen.
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
		// r is the required buffer size in characters including the null terminator
		if r > maxPathLen {
			return "", fmt.Errorf("file handle path exceeds maximum allowed length (%d > %d)", r, maxPathLen)
		}
		buf = make([]uint16, r)
	}
}

// VerifyFileHandleInRoot verifies that the open file descriptor points within
// canonicalWorkDir and does not access restricted paths. It is the exported
// seam used by the server's push snapshot reader, which must validate a
// descriptor it opened through its own os.Root (08-REVIEW CR-02).
func VerifyFileHandleInRoot(f *os.File, canonicalWorkDir string) error {
	return verifyFileHandle(f, canonicalWorkDir)
}

// verifyFileHandle queries the Windows kernel for the normalized target path
// of the open handle, ensuring that the file held open actually resides within
// canonicalWorkDir and does not access restricted paths such as .git.
func verifyFileHandle(f *os.File, canonicalWorkDir string) error {
	finalPath, err := getFinalPathName(syscall.Handle(f.Fd()))
	if err != nil {
		return err
	}

	// Strip Windows extended path prefix \\?\ or \\?\UNC\
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
