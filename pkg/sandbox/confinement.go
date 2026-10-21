package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ConfinementRank names the single filesystem confinement layer that governs
// one direct-execution run. The chain is ordered RankBwrapRO then
// RankLandlock; exactly one rank is selected per run, never both, so any
// denial has a single identifiable owner (CONFINE-01).
type ConfinementRank string

const (
	// RankBwrapRO is bubblewrap with a private read-only host root. It also
	// removes the network interface.
	RankBwrapRO ConfinementRank = "bwrap-ro"
	// RankLandlock is the helper's Landlock filesystem rule set. It leaves
	// the network interface in place, so NetworkIsolated is false.
	RankLandlock ConfinementRank = "landlock"
)

// ErrConfinementUnavailable is the typed sentinel returned when no
// filesystem confinement rank can be attested. Callers MUST map it to
// StatusUnavailable with zero commands executed, never to StatusPassed and
// never to an unconfined execution (CONFINE-01 fail-closed).
var ErrConfinementUnavailable = errors.New("no filesystem confinement rank is available")

// ConfinementUnavailableLabel is the exact fail-closed label carried when no
// filesystem confinement rank attested. It is deliberately distinct from
// ReducedGuaranteeLabel, which states the NETWORK claim only: the filesystem
// and network axes must stay independently readable (T-08-09).
const ConfinementUnavailableLabel = "CONFINEMENT-UNAVAILABLE: no filesystem confinement rank attested; zero commands executed"

// WrapOptions carries the optional per-call inputs a rank's argv builder
// needs. GatewaySocketPath/HelperPath are non-empty only for stage-0
// dependency preparation, which needs the restricted UDS gateway and the
// trusted sibling helper inside the sandbox.
type WrapOptions struct {
	GatewaySocketPath string
	HelperPath        string
}

// ConfinementSpec is the single resolved confinement decision for a whole
// run. Every stage — stage-0 dependency preparation, build, test and in-loop
// commands — derives its argv and env from this one value.
type ConfinementSpec struct {
	Rank            ConfinementRank
	NetworkIsolated bool
	// WrapArgv returns the exact argv to execute for one stage. A non-nil
	// error means the stage must not run: the caller maps it to
	// StatusUnavailable rather than executing unwrapped.
	WrapArgv func(argv []string, paths DirectJobPaths, opts WrapOptions) ([]string, error)
	// Env returns the environment for one stage. Ranks that carry their
	// environment inside the wrapper return baseEnv unchanged.
	Env func(baseEnv []string, paths DirectJobPaths) []string
}

// confinementProbes parameterises the rank decision so precedence and
// fail-closed behaviour are unit-testable without bubblewrap or Landlock.
type confinementProbes struct {
	goos          string
	attestBwrapRO func(context.Context) error
	probeLandlock func(context.Context) error
}

// resolveConfinementRankWith is the pure, unmemoized rank decision. It is the
// only place the chain order lives: bwrap-ro first, then landlock, then a
// typed ErrConfinementUnavailable. It never returns a spec together with a
// non-nil error, and it never returns a passthrough spec — a host with no
// usable rank is refused rather than run unconfined.
func resolveConfinementRankWith(ctx context.Context, p confinementProbes) (ConfinementSpec, error) {
	if p.goos != "linux" {
		return ConfinementSpec{}, fmt.Errorf(
			"%w: direct execution requires Linux filesystem confinement (host %q has no supported rank)",
			ErrConfinementUnavailable, p.goos)
	}
	bwrapErr := p.attestBwrapRO(ctx)
	if bwrapErr == nil {
		return bwrapROConfinementSpec(), nil
	}
	landlockErr := p.probeLandlock(ctx)
	if landlockErr == nil {
		return landlockConfinementSpec(), nil
	}
	return ConfinementSpec{}, fmt.Errorf(
		"%w: bwrap-ro: %v; landlock: %v",
		ErrConfinementUnavailable, bwrapErr, landlockErr)
}

// bwrapROConfinementSpec builds the bwrap-ro rank: the read-only host root
// argv builder in bwrap_ro.go plus a no-op env (this rank carries its
// environment inside the wrapper's --clearenv/--setenv block, which is derived
// from the same directRootEnvPins source as buildDirectStageEnv).
func bwrapROConfinementSpec() ConfinementSpec {
	return ConfinementSpec{
		Rank:            RankBwrapRO,
		NetworkIsolated: true,
		WrapArgv:        BuildBwrapROStageArgv,
		Env: func(baseEnv []string, paths DirectJobPaths) []string {
			return baseEnv
		},
	}
}

// landlockConfinementSpec builds the landlock rank: the trusted helper's
// landlock-run subcommand owns enforcement (the server never calls
// restrict_self, because Landlock domains are per-thread and irreversible).
// This rank remaps no filesystem, so its roots are HOST paths — the same-path
// contract Phase 9 established — and it removes no network interface, so
// NetworkIsolated is false; Phase 11 supplies the host-root env here.
//
// WrapArgv resolves the helper itself when WrapOptions.HelperPath is empty, so
// VerifyDirGated's build and test call sites can keep passing WrapOptions{}.
func landlockConfinementSpec() ConfinementSpec {
	return ConfinementSpec{
		Rank:            RankLandlock,
		NetworkIsolated: false,
		WrapArgv: func(argv []string, paths DirectJobPaths, opts WrapOptions) ([]string, error) {
			helper := strings.TrimSpace(opts.HelperPath)
			if helper == "" {
				resolved, err := ResolveSandboxHelperPath()
				if err != nil {
					return nil, err
				}
				helper = resolved
			}

			// The helper receives exactly the roots the server already pinned
			// in the stage environment: the read-only host root plus the four
			// job roots. A root that is empty, "/" or relative would move the
			// rank's meaning, so it is refused rather than guessed.
			roots := []string{paths.WorkspaceDir, paths.CacheDir, paths.TmpDir, paths.HomeDir}

			// Stage-0 dependency preparation reaches this job's restricted UDS
			// gateway. The landlock rank does not remap the filesystem, so the
			// helper is handed the HOST socket path and needs an explicit --rw
			// grant on its parent directory; the bwrap-only in-sandbox
			// placeholders in the argv are rewritten to those host paths
			// (11-REVIEW CR-01).
			sock := strings.TrimSpace(opts.GatewaySocketPath)
			if sock != "" {
				sock = filepath.Clean(sock)
				if !filepath.IsAbs(sock) {
					return nil, fmt.Errorf(
						"%w: landlock stage-0 gateway socket must be an absolute path, got %q",
						ErrConfinementUnavailable, sock)
				}
				sockDir := filepath.Dir(sock)
				if sockDir == "" || sockDir == "." || sockDir == "/" {
					return nil, fmt.Errorf(
						"%w: landlock stage-0 gateway socket directory must not be the filesystem root, got %q",
						ErrConfinementUnavailable, sockDir)
				}
				roots = append(roots, sockDir)
			}

			deduped := make([]string, 0, len(roots))
			seen := make(map[string]bool, len(roots))
			for _, root := range roots {
				if root == "" || root == "/" || !filepath.IsAbs(root) {
					return nil, fmt.Errorf(
						"%w: landlock rank requires an absolute non-root job root, got %q",
						ErrConfinementUnavailable, root)
				}
				if seen[root] {
					continue
				}
				seen[root] = true
				deduped = append(deduped, root)
			}

			// 4 fixed tokens (helper, landlock-run, --ro /) + 2 per root +
			// the "--" separator, plus the command itself (IN-04).
			wrapped := make([]string, 0, len(argv)+5+2*len(deduped))
			wrapped = append(wrapped, helper, "landlock-run", "--ro", "/")
			for _, root := range deduped {
				wrapped = append(wrapped, "--rw", root)
			}
			wrapped = append(wrapped, "--")
			for _, el := range argv {
				switch {
				case el == BwrapROHelperPath:
					wrapped = append(wrapped, helper)
				case el == BwrapGatewaySocketPath && sock != "":
					wrapped = append(wrapped, sock)
				default:
					wrapped = append(wrapped, el)
				}
			}
			return wrapped, nil
		},
		Env: func(baseEnv []string, paths DirectJobPaths) []string {
			return baseEnv
		},
	}
}

// applyHelperExitToReport maps a landlock helper's pre-exec exit code onto the
// run's fail-closed verdict. It returns true when it claimed the stage result:
// a returned 70–76 code means the target never ran, so it must never reach
// mapDirectStageFailure and become a build/test failure (T-10-06).
//
// The split is now honest: exit 74 (the negative control) proves the domain was
// NOT enforcing and becomes StatusConfinementDenied, never StatusUnavailable;
// 70/71/72/73/75/76 stay StatusUnavailable because they are environment or
// helper-configuration faults, not project verdicts. The claimed stage is
// recorded on the report so reviewers see which command never ran (IN-02).
func applyHelperExitToReport(report *VerificationReport, rank ConfinementRank, stage string, res *StageResult) bool {
	if report == nil || res == nil || res.Passed || rank != RankLandlock {
		return false
	}
	if helperExitConfinementDenied(res.ExitCode) {
		reason, _ := HelperExitReason(HelperExitNegativeControl)
		report.FailedStage = stage
		report.Status = StatusConfinementDenied
		report.Reason = fmt.Sprintf("%s (helper exit %d)", reason, res.ExitCode)
		report.Summary = "CONFINEMENT-DENIED: " + report.Reason
		report.ConfinementDeniedPath = ""
		report.ConfinementAdvisory = ""
		// WorkspaceDir/CommitSHA/DetectedType/ConfinementRank/
		// ConfinementNetworkIsolated are already set on report by the caller.
		return true
	}
	reason, ok := helperExitUnavailable(res.ExitCode)
	if !ok {
		return false
	}
	report.FailedStage = stage
	report.Status = StatusUnavailable
	report.Reason = fmt.Sprintf("%s (helper exit %d)", reason, res.ExitCode)
	report.Summary = "UNAVAILABLE: " + report.Reason
	// WorkspaceDir/CommitSHA/DetectedType/ConfinementRank/
	// ConfinementNetworkIsolated are already set on report by the caller.
	return true
}

// appendConfinementAdvisory attaches an indeterminate classification's
// advisory to the report summary exactly once. It never changes Status: an
// ambiguous message must ride the ordinary build/test verdict rather than
// becoming a security denial.
func appendConfinementAdvisory(report *VerificationReport) {
	if report == nil || report.ConfinementAdvisory == "" {
		return
	}
	marker := " (confinement advisory: "
	if strings.Contains(report.Summary, marker) {
		return
	}
	report.Summary += marker + report.ConfinementAdvisory + ")"
}

// applyDenialClassification runs the pure three-valued classifier over one
// failed stage and applies its verdict. It returns true only when a denial is
// PROVEN, in which case the caller must return immediately with
// StatusConfinementDenied and never reach the generic stage mapping (T-11-04).
//
// An Indeterminate verdict sets ConfinementAdvisory and returns false without
// touching Status, so the ordinary build/test verdict stands (T-11-05). This
// is the single denial owner for a run; InLoopCommand deliberately returns raw
// output with no verdict.
func applyDenialClassification(report *VerificationReport, rank ConfinementRank, paths DirectJobPaths, stage string, res *StageResult) bool {
	if report == nil || res == nil {
		return false
	}
	classification := ClassifyConfinementDenial(DenialInput{
		Rank:       rank,
		ExitCode:   res.ExitCode,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		WriteRoots: directWriteRoots(paths),
	})
	switch classification.Verdict {
	case DenialProvenDenied:
		report.FailedStage = stage
		report.Status = StatusConfinementDenied
		report.Reason = classification.Reason
		if classification.Path != "" {
			report.Reason = fmt.Sprintf("%s (path: %s)", report.Reason, classification.Path)
		}
		report.Summary = "CONFINEMENT-DENIED: " + report.Reason
		report.ConfinementDeniedPath = classification.Path
		report.ConfinementAdvisory = ""
		return true
	case DenialIndeterminate:
		if classification.Advisory != "" {
			report.ConfinementAdvisory = classification.Advisory
		}
		return false
	default:
		return false
	}
}

// confinementResolverOverride is the test seam: when set, ResolveConfinementRank
// returns its result without touching the kernel probes or the memoized value.
var (
	confinementOverrideMu       sync.RWMutex
	confinementResolverOverride func(context.Context) (ConfinementSpec, error)
)

// setConfinementResolverOverrideForTest installs the test override.
func setConfinementResolverOverrideForTest(fn func(context.Context) (ConfinementSpec, error)) {
	confinementOverrideMu.Lock()
	defer confinementOverrideMu.Unlock()
	confinementResolverOverride = fn
}

// resetConfinementRankForTest clears the test override.
func resetConfinementRankForTest() {
	confinementOverrideMu.Lock()
	defer confinementOverrideMu.Unlock()
	confinementResolverOverride = nil
}

var (
	confinementResolveOnce  sync.Once
	confinementResolvedSpec ConfinementSpec
	confinementResolvedErr  error
)

// detachedProbeTimeout bounds the process-lifetime rank probe so a detached
// probe can never hang the first request forever.
const detachedProbeTimeout = 30 * time.Second

// detachedProbeContext returns the context the memoized rank probe runs under.
// It detaches from the caller's cancellation (context.WithoutCancel) and adds
// its own bounded timeout, so a cancelled/expired first request cannot poison
// the process-wide rank decision until server restart (11-REVIEW WR-01).
func detachedProbeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), detachedProbeTimeout)
}

// ResolveConfinementRank is the single exported confinement decision function.
// Both direct exec call sites — VerifyDirGated and InLoopCommand — route
// through it. The real rank decision runs once per process and memoizes both
// the spec and the error: kernel and LSM capability is process-invariant,
// while enforcement is proved per execution. A test override bypasses the
// memoized value entirely.
func ResolveConfinementRank(ctx context.Context) (ConfinementSpec, error) {
	confinementOverrideMu.RLock()
	override := confinementResolverOverride
	confinementOverrideMu.RUnlock()
	if override != nil {
		return override(ctx)
	}
	confinementResolveOnce.Do(func() {
		probeCtx, cancel := detachedProbeContext(ctx)
		defer cancel()
		confinementResolvedSpec, confinementResolvedErr = resolveConfinementRankWith(probeCtx, confinementProbes{
			goos:          runtime.GOOS,
			attestBwrapRO: AttestBwrapRO,
			probeLandlock: probeLandlockRank,
		})
	})
	return confinementResolvedSpec, confinementResolvedErr
}

// ConfinementUnavailableReport builds the typed fail-closed report for the
// exhausted-rank path. It always carries StatusUnavailable with
// ConfinementUnavailableLabel and a non-nil empty Results slice, never
// StatusPassed and never the network ReducedGuaranteeLabel.
func ConfinementUnavailableReport(reason string) *VerificationReport {
	detail := strings.TrimSpace(reason)
	switch {
	case detail == "":
		detail = ConfinementUnavailableLabel
	case !strings.Contains(detail, ConfinementUnavailableLabel):
		detail = ConfinementUnavailableLabel + ": " + detail
	}
	return &VerificationReport{
		Status:  StatusUnavailable,
		Reason:  detail,
		Summary: "UNAVAILABLE: " + detail,
		Results: make([]ExecutionResult, 0),
	}
}

// confinementWrapFailureReport maps a spec WrapArgv failure onto the run's
// existing report identity. A wrap failure means the stage cannot be
// confined, so the run is refused with zero new commands rather than
// executed unwrapped (T-08-06).
func confinementWrapFailureReport(report *VerificationReport, err error) *VerificationReport {
	degraded := ConfinementUnavailableReport(err.Error())
	degraded.WorkspaceDir = report.WorkspaceDir
	degraded.CommitSHA = report.CommitSHA
	degraded.DetectedType = report.DetectedType
	degraded.ConfinementRank = report.ConfinementRank
	degraded.ConfinementNetworkIsolated = report.ConfinementNetworkIsolated
	degraded.ConfinementDeniedPath = report.ConfinementDeniedPath
	degraded.ConfinementAdvisory = report.ConfinementAdvisory
	return degraded
}
