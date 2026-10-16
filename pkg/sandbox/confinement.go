package sandbox

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
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

// bwrapROConfinementSpec builds the bwrap-ro rank: the private-root argv
// builder in direct_isolation.go plus a no-op env (this rank carries its
// environment inside the wrapper's --clearenv/--setenv block).
func bwrapROConfinementSpec() ConfinementSpec {
	return ConfinementSpec{
		Rank:            RankBwrapRO,
		NetworkIsolated: true,
		WrapArgv: func(argv []string, paths DirectJobPaths, opts WrapOptions) ([]string, error) {
			return directIsolatedArgv(paths.WorkspaceDir, paths.CacheDir, paths.TmpDir, argv, opts.GatewaySocketPath, opts.HelperPath)
		},
		Env: func(baseEnv []string, paths DirectJobPaths) []string {
			return baseEnv
		},
	}
}

// landlockConfinementSpec builds the landlock rank: the trusted helper's
// landlock-run subcommand owns enforcement (the server never calls
// restrict_self, because Landlock domains are per-thread and irreversible).
// This rank remaps no filesystem and removes no network interface, so
// NetworkIsolated is false; Phase 11 supplies the host-root env here.
func landlockConfinementSpec() ConfinementSpec {
	return ConfinementSpec{
		Rank:            RankLandlock,
		NetworkIsolated: false,
		WrapArgv: func(argv []string, paths DirectJobPaths, opts WrapOptions) ([]string, error) {
			helper := strings.TrimSpace(opts.HelperPath)
			if helper == "" {
				return nil, fmt.Errorf("%w: landlock rank requires the trusted helper path", ErrConfinementUnavailable)
			}
			wrapped := make([]string, 0, len(argv)+2)
			wrapped = append(wrapped, helper, "landlock-run")
			return append(wrapped, argv...), nil
		},
		Env: func(baseEnv []string, paths DirectJobPaths) []string {
			return baseEnv
		},
	}
}

// attestBwrapRO is the bwrap-ro rank probe. Phase 9 replaces this body with
// the real `--ro-bind / /` shape probe in bwrap_ro.go; until then the
// existing userns+net attestation is the honest proxy.
func attestBwrapRO(ctx context.Context) error {
	return AttestBwrap(ctx)
}

// probeLandlockRank is the landlock rank probe. Phase 10/11 replaces this
// body once the trusted helper grows its landlock-run subcommand; until then
// the rank is honestly unavailable rather than silently degraded.
func probeLandlockRank(ctx context.Context) error {
	_ = ctx
	return fmt.Errorf("%w: landlock: helper landlock-run subcommand not yet available", ErrConfinementUnavailable)
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
		confinementResolvedSpec, confinementResolvedErr = resolveConfinementRankWith(ctx, confinementProbes{
			goos:          runtime.GOOS,
			attestBwrapRO: attestBwrapRO,
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
	return degraded
}
