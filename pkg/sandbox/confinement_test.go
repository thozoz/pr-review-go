package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zeroConfinementSpec reports whether a resolution failure returned the
// zero-value spec (a struct with closure fields cannot be compared with ==).
func zeroConfinementSpec(spec ConfinementSpec) bool {
	return spec.Rank == "" && spec.WrapArgv == nil && spec.Env == nil && !spec.NetworkIsolated
}

// containsTriple reports whether argv contains the exact ordered triple a,b,c.
func containsTriple(argv []string, a, b, c string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b && argv[i+2] == c {
			return true
		}
	}
	return false
}

// TestConfinementRankPrecedence pins the chain order and the fail-closed
// exhaustion: bwrap-ro wins first, landlock is the fallback, and a host with
// no usable rank — including any non-Linux host — is refused rather than run
// unconfined (T-08-06).
func TestConfinementRankPrecedence(t *testing.T) {
	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errors.New("probe refused") }

	cases := []struct {
		name     string
		goos     string
		bwrap    func(context.Context) error
		landlock func(context.Context) error
		wantRank ConfinementRank
		wantErr  bool
	}{
		{"bwrap-first", "linux", ok, ok, RankBwrapRO, false},
		{"landlock-fallback", "linux", fail, ok, RankLandlock, false},
		{"all-unavailable", "linux", fail, fail, "", true},
		{"darwin-refused", "darwin", ok, ok, "", true},
		{"windows-refused", "windows", ok, ok, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := resolveConfinementRankWith(context.Background(), confinementProbes{
				goos:          tc.goos,
				attestBwrapRO: tc.bwrap,
				probeLandlock: tc.landlock,
			})
			if tc.wantErr {
				if !errors.Is(err, ErrConfinementUnavailable) {
					t.Fatalf("want ErrConfinementUnavailable, got %v", err)
				}
				if !zeroConfinementSpec(spec) {
					t.Fatalf("failed resolution must return a zero spec, got %+v", spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected resolution error: %v", err)
			}
			if spec.Rank != tc.wantRank {
				t.Fatalf("rank = %q, want %q", spec.Rank, tc.wantRank)
			}
		})
	}
}

// TestConfinementRankIsSingleLayer proves no run stacks two layers: a
// bwrap-ro argv never carries the landlock-run helper, and a landlock argv
// never carries bwrap, so any denial has exactly one owner (T-08-07).
func TestConfinementRankIsSingleLayer(t *testing.T) {
	paths := provisionTestJobPaths(t)
	sample := []string{"go", "build", "./..."}

	bwrapArgv, err := bwrapROConfinementSpec().WrapArgv(sample, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("bwrap-ro wrap: %v", err)
	}
	bwrapJoined := strings.Join(bwrapArgv, " ")
	if !strings.Contains(bwrapJoined, "bwrap") {
		t.Fatalf("bwrap-ro argv must invoke bwrap, got %q", bwrapJoined)
	}
	if strings.Contains(bwrapJoined, "landlock-run") {
		t.Fatalf("bwrap-ro argv must not also stack landlock, got %q", bwrapJoined)
	}

	landlockArgv, err := landlockConfinementSpec().WrapArgv(sample, paths, WrapOptions{HelperPath: "/opt/pr-review-sandbox-helper"})
	if err != nil {
		t.Fatalf("landlock wrap: %v", err)
	}
	landlockJoined := strings.Join(landlockArgv, " ")
	if !strings.Contains(landlockJoined, "landlock-run") {
		t.Fatalf("landlock argv must invoke the helper's landlock-run subcommand, got %q", landlockJoined)
	}
	if strings.Contains(landlockJoined, "bwrap") {
		t.Fatalf("landlock argv must not also stack bwrap, got %q", landlockJoined)
	}
}

// TestConfinementSpecArgvDerivesFromJobPaths proves both ranks consume the
// same DirectJobPaths value: the bwrap-ro argv binds exactly the job's
// workspace, cache and tmp onto /work, /cache and /tmp, plus the stage-0
// gateway socket and helper (T-08-08).
func TestConfinementSpecArgvDerivesFromJobPaths(t *testing.T) {
	paths := provisionTestJobPaths(t)
	opts := WrapOptions{GatewaySocketPath: "/tmp/gw.sock", HelperPath: "/tmp/helper"}
	argv, err := bwrapROConfinementSpec().WrapArgv(
		[]string{"/sandbox-helper", "prepare", "-socket", BwrapGatewaySocketPath, "-work", "/work"},
		paths,
		opts,
	)
	if err != nil {
		t.Fatalf("stage-0 wrap: %v", err)
	}

	resolved := func(p string) string {
		t.Helper()
		got, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", p, err)
		}
		return got
	}
	for _, tc := range []struct{ source, target string }{
		{resolved(paths.WorkspaceDir), "/work"},
		{resolved(paths.CacheDir), "/cache"},
		{resolved(paths.TmpDir), "/tmp"},
	} {
		if !containsTriple(argv, "--bind", tc.source, tc.target) {
			t.Errorf("argv must bind job path %q onto %q: %q", tc.source, tc.target, argv)
		}
	}
	if !containsTriple(argv, "--ro-bind", opts.GatewaySocketPath, BwrapGatewaySocketPath) {
		t.Errorf("argv must bind gateway socket %q onto %q: %q", opts.GatewaySocketPath, BwrapGatewaySocketPath, argv)
	}
	if !containsTriple(argv, "--ro-bind", opts.HelperPath, "/sandbox-helper") {
		t.Errorf("argv must bind helper %q onto /sandbox-helper: %q", opts.HelperPath, argv)
	}
}

// TestResolveConfinementRankAllUnavailableIsFailClosed is Phase 8 success
// criterion 3's regression test: with every rank forced unavailable a direct
// verification run executes zero commands, never reaches StatusPassed, and
// reports the typed sentinel with the confinement label rather than the
// network label. It must not require bubblewrap or Landlock.
func TestResolveConfinementRankAllUnavailableIsFailClosed(t *testing.T) {
	setConfinementResolverOverrideForTest(func(context.Context) (ConfinementSpec, error) {
		return ConfinementSpec{}, fmt.Errorf("%w: test forced every rank unavailable", ErrConfinementUnavailable)
	})
	defer resetConfinementRankForTest()

	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/confinementfailclosed\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	d := NewDirectRunnerWithRoots(time.Minute, DirectRoots{WorkRoot: t.TempDir(), CacheRoot: t.TempDir()})
	report, err := d.VerifyDirGated(context.Background(), dir, directTestSHA, false)
	if err != nil {
		t.Fatalf("VerifyDirGated returned a caller error: %v", err)
	}
	if report.Status == StatusPassed {
		t.Fatal("an exhausted confinement chain must never reach StatusPassed")
	}
	if report.Status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", report.Status, StatusUnavailable)
	}
	if !strings.Contains(report.Reason, ConfinementUnavailableLabel) {
		t.Errorf("reason must carry the confinement-unavailable label: %q", report.Reason)
	}
	if !strings.Contains(report.Reason, ErrConfinementUnavailable.Error()) {
		t.Errorf("reason must carry the typed sentinel text: %q", report.Reason)
	}
	if strings.Contains(report.Reason, ReducedGuaranteeLabel) {
		t.Errorf("confinement failure must not be reported as a network reduced guarantee: %q", report.Reason)
	}
	if len(report.Results) != 0 {
		t.Errorf("zero commands may run when every rank is unavailable, got %d results", len(report.Results))
	}
}

// TestConfinementUnavailableLabelIsDistinct keeps the filesystem and network
// claims independently readable (T-08-09).
func TestConfinementUnavailableLabelIsDistinct(t *testing.T) {
	if ConfinementUnavailableLabel == ReducedGuaranteeLabel {
		t.Fatal("the confinement and network labels must be distinct constants")
	}
	report := ConfinementUnavailableReport("probe")
	if report.Status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", report.Status, StatusUnavailable)
	}
	if !strings.Contains(report.Reason, ConfinementUnavailableLabel) {
		t.Errorf("reason must carry ConfinementUnavailableLabel: %q", report.Reason)
	}
	if strings.Contains(report.Reason, ReducedGuaranteeLabel) || strings.Contains(report.Summary, ReducedGuaranteeLabel) {
		t.Errorf("confinement report must not carry the network label: reason=%q summary=%q", report.Reason, report.Summary)
	}
	if report.Results == nil {
		t.Error("Results must be a non-nil empty slice")
	}
	if len(report.Results) != 0 {
		t.Errorf("Results must be empty, got %d", len(report.Results))
	}
}

// TestOneConfinementDecisionFunction structurally forbids a second decision
// path: raw AttestBwrap/directIsolatedArgv calls may only appear in the rank
// probe/argv-builder files, and direct.go must route through
// ResolveConfinementRank (T-08-10).
func TestOneConfinementDecisionFunction(t *testing.T) {
	allowedAttest := map[string]bool{"bwrap.go": true, "confinement.go": true}
	allowedIsolated := map[string]bool{"direct_isolation.go": true, "confinement.go": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		if strings.Contains(src, "AttestBwrap(") && !allowedAttest[name] {
			t.Errorf("%s must not call AttestBwrap( directly; route through ResolveConfinementRank", name)
		}
		if strings.Contains(src, "directIsolatedArgv(") && !allowedIsolated[name] {
			t.Errorf("%s must not call directIsolatedArgv( directly; route through ResolveConfinementRank", name)
		}
	}

	directSrc, err := os.ReadFile("direct.go")
	if err != nil {
		t.Fatalf("read direct.go: %v", err)
	}
	if strings.Contains(string(directSrc), "AttestBwrap(") || strings.Contains(string(directSrc), "directIsolatedArgv(") {
		t.Error("direct.go must route every stage through the single resolved ConfinementSpec")
	}
	if !strings.Contains(string(directSrc), "ResolveConfinementRank") {
		t.Error("direct.go must call ResolveConfinementRank in both exec call sites")
	}
}

// TestInLoopCommandRoutesThroughConfinementDispatch proves the in-loop exec
// entry shares the single decision function and fails closed with no output.
func TestInLoopCommandRoutesThroughConfinementDispatch(t *testing.T) {
	setConfinementResolverOverrideForTest(func(context.Context) (ConfinementSpec, error) {
		return ConfinementSpec{}, fmt.Errorf("%w: test forced every rank unavailable", ErrConfinementUnavailable)
	})
	defer resetConfinementRankForTest()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent on PATH")
	}
	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/inloopdispatch\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	stdout, stderr, code, err := InLoopCommand(context.Background(), dir, []string{"go", "build", "./..."}, time.Minute, 0,
		WithDirectRoots(DirectRoots{WorkRoot: t.TempDir(), CacheRoot: t.TempDir()}))
	if !errors.Is(err, ErrConfinementUnavailable) {
		t.Fatalf("in-loop command must fail closed through the resolver, got code=%d err=%v", code, err)
	}
	if code != -1 {
		t.Fatalf("exit code = %d, want -1", code)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("no output may be produced before confinement resolves: stdout=%q stderr=%q", stdout, stderr)
	}
}
