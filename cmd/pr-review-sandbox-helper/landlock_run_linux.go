//go:build linux

// The Landlock applier. This is the ONLY code in the repository that calls
// restrict_self: a Landlock domain is per-thread and irreversible, so an
// in-server call would either sandbox the long-lived server or restrict a
// thread the target exec never runs on (T-10-01, T-10-02).
//
// Shape: parse → LockOSThread → ABI query → ABI-sized ruleset → read-only
// rules → read-write rules → no_new_privs → restrict_self → positive control →
// negative control → syscall.Exec. There is no fork, no supervisor and no
// second stage; the pinned thread becomes the target's main thread carrying the
// domain.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

// errNegativeControlWritable is the typed security denial raised when a root
// the caller declared read-only accepted a create. It is deliberately a
// distinct type so a caller can tell a denial from an ordinary I/O failure.
type errNegativeControlWritable struct {
	root string
}

func (e *errNegativeControlWritable) Error() string {
	return fmt.Sprintf("read-only root %q was writable after restriction; the domain is not enforcing", e.root)
}

// runLandlockRun applies the Landlock domain to this thread and execs the
// target. Every failure returns one of the disjoint 70–76 pre-exec codes and
// prints a helper-authored reason to stderr. On success this function never
// returns.
func runLandlockRun(args []string) int {
	// 1. Parse before anything else: a usage error must not touch the kernel.
	opts, err := parseLandlockRunArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return sandbox.HelperExitUsage
	}

	// 2. Resolve the target BEFORE restricting, so PATH resolution cannot be
	// affected by the rule set, and a missing toolchain is a typed 76 rather
	// than a confusing EACCES under the domain (T-10-10).
	resolved, err := exec.LookPath(opts.command[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "landlock-run: cannot resolve target command %q: %v\n", opts.command[0], err)
		return sandbox.HelperExitExec
	}
	targetArgv := append([]string{resolved}, opts.command[1:]...)

	// 3. Pin this goroutine to its OS thread and never unlock it.
	//
	// Landlock domains are per-thread and irrevocable. GOMAXPROCS is not a
	// substitute: without LockOSThread the runtime may migrate this goroutine
	// to another thread between restrict_self and execve, and the target would
	// run on a thread with no domain.
	runtime.LockOSThread()

	// 4. Query the Landlock ABI version.
	abi, err := queryLandlockABI()
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) ||
			errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) {
			fmt.Fprintf(os.Stderr, "landlock-run: Landlock is unavailable on this host: %v\n", err)
			return sandbox.HelperExitUnavailable
		}
		fmt.Fprintf(os.Stderr, "landlock-run: Landlock ABI query failed: %v\n", err)
		return sandbox.HelperExitUnavailable
	}
	if !sandbox.LandlockRankSupported(abi) {
		fmt.Fprintf(os.Stderr, "landlock-run: observed Landlock ABI %d is below the minimum usable ABI %d\n",
			abi, sandbox.MinLandlockABI)
		return sandbox.HelperExitUnavailable
	}

	// 5. Create the ABI-sized ruleset.
	mask := sandbox.HandledAccessFS(abi)
	rulesetFd, err := createLandlockRuleset(mask)
	if err != nil {
		fmt.Fprintf(os.Stderr, "landlock-run: could not create ruleset for ABI %d (mask %#x): %v\n", abi, mask, err)
		return sandbox.HelperExitRuleset
	}

	// 6. Add the path rules. Read-only roots first, then read-write.
	roAccess := sandbox.AllowedReadOnlyAccess(abi)
	rwAccess := sandbox.AllowedReadWriteAccess(abi)
	for _, root := range opts.ro {
		if roAccess == 0 {
			continue
		}
		if err := addLandlockPathRule(rulesetFd, root, roAccess); err != nil {
			_ = unix.Close(rulesetFd)
			fmt.Fprintf(os.Stderr, "landlock-run: read-only rule for %q failed: %v\n", root, err)
			return sandbox.HelperExitRuleset
		}
	}
	for _, root := range opts.rw {
		if rwAccess == 0 {
			continue
		}
		if err := addLandlockPathRule(rulesetFd, root, rwAccess); err != nil {
			_ = unix.Close(rulesetFd)
			fmt.Fprintf(os.Stderr, "landlock-run: read-write rule for %q failed: %v\n", root, err)
			return sandbox.HelperExitRuleset
		}
	}

	// 7. Establish the domain. no_new_privs is per-thread and irreversible.
	if err := applyLandlockDomain(rulesetFd); err != nil {
		_ = unix.Close(rulesetFd)
		fmt.Fprintf(os.Stderr, "landlock-run: %v\n", err)
		return sandbox.HelperExitDomainEstablish
	}
	// The domain now holds a reference; no pre-sandbox descriptor survives
	// into the target (T-10-08).
	_ = unix.Close(rulesetFd)

	// 8. Pre-exec controls: prove the domain is both non-empty and enforcing.
	if err := positiveControl(opts.rw); err != nil {
		fmt.Fprintf(os.Stderr, "landlock-run: positive control failed: %v\n", err)
		return sandbox.HelperExitPositiveControl
	}
	if err := negativeControl(opts.ro); err != nil {
		fmt.Fprintf(os.Stderr, "landlock-run: negative control failed: %v\n", err)
		return sandbox.HelperExitNegativeControl
	}

	// 9. Hand the restricted thread to the target. execve destroys the sibling
	// threads, so the calling thread becomes the target's main thread carrying
	// the domain. This is the only correct shape before ABI v8 (no TSYNC).
	if err := syscall.Exec(targetArgv[0], targetArgv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "landlock-run: exec %q failed: %v\n", targetArgv[0], err)
		return sandbox.HelperExitExec
	}
	// Unreachable: a successful execve does not return.
	return sandbox.HelperExitExec
}

// applyLandlockDomain sets no_new_privs and restricts this thread to the
// ruleset. Both steps are irreversible for the calling thread, which is why
// the helper exists at all: the server must never do this to itself.
func applyLandlockDomain(rulesetFd int) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS failed: %w", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFd), 0, 0); errno != 0 {
		return fmt.Errorf("restrict_self failed: %w", errno)
	}
	return nil
}

// queryLandlockABI returns the Landlock ABI version, treating the
// "Landlock is absent" errnos as typed errors the caller maps to 70.
func queryLandlockABI() (int, error) {
	// unix.Syscall (not RawSyscall) so the runtime can still handle signals.
	r1, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0, 0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION),
	)
	if errno != 0 {
		return 0, errno
	}
	if r1 == ^uintptr(0) {
		return 0, unix.EINVAL
	}
	return int(r1), nil
}

// createLandlockRuleset creates the ruleset with the smallest attr size the
// kernel accepts. `unix.LandlockRulesetAttr` is 24 bytes (Access_fs,
// Access_net, Scoped), but older kernels accept only the prefix they know, so
// the call walks a 24 → 16 → 8 ladder on E2BIG/EINVAL.
func createLandlockRuleset(mask uint64) (int, error) {
	var buf [24]byte
	binary.LittleEndian.PutUint64(buf[0:8], mask) // Access_fs
	binary.LittleEndian.PutUint64(buf[8:16], 0)   // Access_net
	binary.LittleEndian.PutUint64(buf[16:24], 0)  // Scoped
	defer runtime.KeepAlive(&buf)

	var attempts []string
	for _, size := range []uintptr{24, 16, 8} {
		r1, _, errno := unix.Syscall(
			unix.SYS_LANDLOCK_CREATE_RULESET,
			uintptr(unsafe.Pointer(&buf[0])),
			size,
			0,
		)
		if errno == 0 {
			return int(r1), nil
		}
		attempts = append(attempts, fmt.Sprintf("size %d: %v", size, errno))
		if !errors.Is(errno, unix.E2BIG) && !errors.Is(errno, unix.EINVAL) {
			return -1, fmt.Errorf("landlock_create_ruleset failed: %v", errno)
		}
	}
	return -1, fmt.Errorf("landlock_create_ruleset rejected every attribute size (%v)", attempts)
}

// addLandlockPathRule grants allowed beneath root. The O_PATH fd is closed
// explicitly on every return path so no pre-sandbox descriptor survives; it is
// opened without O_NOFOLLOW on purpose, because Debian's merged-/usr symlinks
// would otherwise yield an unusable fd.
func addLandlockPathRule(rulesetFd int, root string, allowed uint64) error {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", root, err)
	}
	pathFd, err := unix.Open(resolved, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open O_PATH %q: %w", resolved, err)
	}

	var attr [12]byte
	binary.LittleEndian.PutUint64(attr[0:8], allowed)         // Allowed_access
	binary.LittleEndian.PutUint32(attr[8:12], uint32(pathFd)) // Parent_fd
	_, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFd),
		uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
		uintptr(unsafe.Pointer(&attr[0])),
	)
	runtime.KeepAlive(&attr)

	closeErr := unix.Close(pathFd)
	if errno != 0 {
		return fmt.Errorf("landlock_add_rule %q (allowed %#x): %v", resolved, allowed, errno)
	}
	return closeErr
}

// positiveControl proves a read-write root really is writable after the
// restriction: a root that exists but is not writable would otherwise fail
// much later as a confusing build error.
func positiveControl(roots []string) error {
	for _, root := range roots {
		f, err := os.CreateTemp(root, ".pr-review-landlock-positive-*")
		if err != nil {
			return fmt.Errorf("read-write root %q is not writable: %w", root, err)
		}
		name := f.Name()
		_, werr := f.Write([]byte("pr-review-landlock-positive-control\n"))
		serr := f.Sync()
		cerr := f.Close()
		rerr := os.Remove(name)
		if werr != nil {
			return fmt.Errorf("read-write root %q rejected a write: %w", root, werr)
		}
		if serr != nil {
			return fmt.Errorf("read-write root %q rejected a sync: %w", root, serr)
		}
		if cerr != nil {
			return fmt.Errorf("read-write root %q rejected a close: %w", root, cerr)
		}
		if rerr != nil {
			return fmt.Errorf("read-write root %q rejected a remove: %w", root, rerr)
		}
	}
	return nil
}

// negativeControl proves a read-only root is NOT writable after the
// restriction. Any ordinary DAC/LSM denial is a pass: the control proves that
// a denial occurred, not which mechanism produced it. Only an actual
// successful create is a failure, because that means the domain is not
// enforcing (exit 74).
func negativeControl(roots []string) error {
	for _, root := range roots {
		f, err := os.CreateTemp(root, ".pr-review-landlock-negative-*")
		if err != nil {
			continue // denied: exactly what the control expects
		}
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		return &errNegativeControlWritable{root: root}
	}
	return nil
}
