package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// landlockProbeDeps is the probe seam: resolution, execution, scratch
// provisioning and the read-only ABI query are injectable so every failure
// mode is testable without a Landlock-capable kernel.
type landlockProbeDeps struct {
	resolve   func() (string, error)
	run       func(ctx context.Context, argv []string) (exitCode int, stderr string, err error)
	mkScratch func() (string, func(), error)
	queryABI  func() (int, error)
}

// defaultLandlockProbeDeps is the production probe environment. The helper is
// run with the real server-to-helper contract; a 10-second bound keeps a hung
// helper from stalling rank resolution.
func defaultLandlockProbeDeps() landlockProbeDeps {
	return landlockProbeDeps{
		resolve:  ResolveSandboxHelperPath,
		queryABI: queryLandlockABI,
		run: func(ctx context.Context, argv []string) (int, string, error) {
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
			stderr := NewLimitedCollector(DefaultMaxRetainedBytes)
			cmd.Stdout = nil
			cmd.Stderr = stderr
			err := cmd.Run()
			exitCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					// A non-zero exit is the contract's normal failure
					// channel, not a runner failure.
					exitCode = exitErr.ExitCode()
					err = nil
				}
			}
			return exitCode, stderr.Formatted("probe"), err
		},
		mkScratch: func() (string, func(), error) {
			dir, err := os.MkdirTemp("", "pr-review-landlock-probe-")
			if err != nil {
				return "", nil, err
			}
			return dir, func() { _ = os.RemoveAll(dir) }, nil
		},
	}
}

// probeLandlockRank is the landlock rank probe. It runs the *production*
// helper contract — `landlock-run --ro / --rw <scratch> -- /bin/true` — so a
// host that can create a ruleset but cannot enforce it reports the rank as
// unavailable instead of running unconfined (T-10-03).
func probeLandlockRank(ctx context.Context) error {
	return probeLandlockRankWith(ctx, defaultLandlockProbeDeps())
}

// probeLandlockRankWith runs the probe against the supplied dependency seam.
//
// The read-only ABI query runs FIRST, before the helper is resolved or spawned:
// a host whose kernel has no usable Landlock reports the typed reason
// (ENOSYS/EOPNOTSUPP/EPERM/too-old) immediately instead of a helper exit that
// conflates four different operator remediations (T-11-03).
//
// Mapping is deliberately conservative over the whole 70–76 contract: 70/71/72
// are the honest "this host cannot confine" codes, 73/75/76 mean the probe
// itself is misconfigured, and 74 means a root declared read-only was writable
// — an unenforced domain must never select the rank.
func probeLandlockRankWith(ctx context.Context, deps landlockProbeDeps) error {
	if deps.queryABI == nil {
		deps.queryABI = queryLandlockABI
	}
	abi, abiErr := deps.queryABI()
	if abiErr != nil {
		// Multi-%w keeps both sentinels reachable: errors.Is finds
		// ErrConfinementUnavailable and errors.As finds *LandlockProbeError,
		// so callers can act on the typed reason without parsing text.
		return fmt.Errorf("%w: %w", ErrConfinementUnavailable, abiErr)
	}
	if reason, ok := LandlockABIVersionReason(abi); !ok {
		return fmt.Errorf("%w: %w", ErrConfinementUnavailable,
			newLandlockProbeError(reason, 0, abi, ""))
	}

	helper, err := deps.resolve()
	if err != nil {
		return fmt.Errorf("%w: landlock probe cannot resolve %s: %v", ErrConfinementUnavailable, HelperBinaryName, err)
	}
	if strings.TrimSpace(helper) == "" {
		return fmt.Errorf("%w: landlock probe resolved an empty helper path", ErrConfinementUnavailable)
	}

	scratch, cleanup, err := deps.mkScratch()
	if err != nil {
		return fmt.Errorf("%w: landlock probe scratch root unavailable: %v", ErrConfinementUnavailable, err)
	}
	defer cleanup()

	// The production contract, never a hand-written flag set.
	argv := []string{helper, "landlock-run", "--ro", "/", "--rw", scratch, "--", "/bin/true"}

	exitCode, stderr, runErr := deps.run(ctx, argv)
	if runErr != nil {
		return fmt.Errorf("%w: landlock probe could not run the helper (%s): %v",
			ErrConfinementUnavailable, helper, runErr)
	}

	abiSuffix := fmt.Sprintf(" (observed Landlock ABI %d)", abi)
	switch exitCode {
	case 0:
		return nil
	case HelperExitUnavailable, HelperExitRuleset, HelperExitDomainEstablish:
		reason, _ := HelperExitReason(exitCode)
		return fmt.Errorf("%w: landlock probe reported exit %d: %s%s%s",
			ErrConfinementUnavailable, exitCode, reason, probeStderrSuffix(stderr), abiSuffix)
	case HelperExitPositiveControl, HelperExitUsage, HelperExitExec:
		return fmt.Errorf("%w: landlock probe reported exit %d, which means the probe invocation is misconfigured%s%s",
			ErrConfinementUnavailable, exitCode, probeStderrSuffix(stderr), abiSuffix)
	case HelperExitNegativeControl:
		return fmt.Errorf("%w: landlock probe reported exit %d: the read-only root was writable; the domain is not enforcing%s%s",
			ErrConfinementUnavailable, exitCode, probeStderrSuffix(stderr), abiSuffix)
	default:
		return fmt.Errorf("%w: landlock probe reported an unmapped exit code %d%s%s",
			ErrConfinementUnavailable, exitCode, probeStderrSuffix(stderr), abiSuffix)
	}
}

// probeStderrSuffix appends the helper's own stderr, bounded by the collector,
// so an operator sees the kernel errno the helper observed.
func probeStderrSuffix(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	return "; helper stderr: " + trimmed
}
