package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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

// TestConfinementSpecArgvDerivesFromJobPaths proves the bwrap-ro rank consumes
// the DirectJobPaths value directly: the host root is read-only, /dev, /proc
// and /tmp are carved out, the job's four roots are bound at their own paths,
// the working directory is pinned into the writable workspace, network removal
// is retained, and stage-0 adds exactly the two runtime binds into the private
// tmpfs (T-08-08, CONFINE-02).
func TestConfinementSpecArgvDerivesFromJobPaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap-ro argv requires Linux mount isolation")
	}
	paths := provisionTestJobPaths(t)
	opts := WrapOptions{GatewaySocketPath: "/host/ctl/gw.sock", HelperPath: "/host/ctl/helper"}
	argv, err := bwrapROConfinementSpec().WrapArgv(
		[]string{BwrapROHelperPath, "prepare", "-socket", BwrapGatewaySocketPath, "-work", paths.WorkspaceDir},
		paths,
		opts,
	)
	if err != nil {
		t.Fatalf("stage-0 wrap: %v", err)
	}

	for _, p := range []string{paths.WorkspaceDir, paths.TmpDir, paths.HomeDir, paths.CacheDir} {
		if !containsTriple(argv, "--bind", p, p) {
			t.Errorf("argv must bind job path %q at its own path: %q", p, argv)
		}
	}
	if !containsTriple(argv, "--ro-bind", "/", "/") {
		t.Errorf("argv must bind the host root read-only: %q", argv)
	}
	if !containsPair(argv, "--dev", "/dev") {
		t.Errorf("argv must carve out /dev: %q", argv)
	}
	if !containsPair(argv, "--proc", "/proc") {
		t.Errorf("argv must carve out /proc: %q", argv)
	}
	if !containsPair(argv, "--tmpfs", "/tmp") {
		t.Errorf("argv must carve out a private /tmp: %q", argv)
	}
	if !containsTriple(argv, "--chdir", paths.WorkspaceDir, "--clearenv") {
		t.Errorf("argv must pin --chdir into the writable workspace: %q", argv)
	}
	if !containsElement(argv, "--unshare-net") {
		t.Errorf("argv must retain network removal: %q", argv)
	}
	if !containsTriple(argv, "--ro-bind", opts.GatewaySocketPath, BwrapGatewaySocketPath) {
		t.Errorf("argv must bind gateway socket %q onto %q: %q", opts.GatewaySocketPath, BwrapGatewaySocketPath, argv)
	}
	if !containsTriple(argv, "--ro-bind", opts.HelperPath, BwrapROHelperPath) {
		t.Errorf("argv must bind helper %q onto %q: %q", opts.HelperPath, BwrapROHelperPath, argv)
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
// path: the real-shape rank probe and the argv builder may only appear in
// their own files, and direct.go must route through ResolveConfinementRank
// (T-08-10).
func TestOneConfinementDecisionFunction(t *testing.T) {
	allowedAttest := map[string]bool{"bwrap.go": true, "confinement.go": true}
	allowedRankFiles := map[string]bool{"bwrap_ro.go": true, "confinement.go": true}

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
		// AttestBwrapRO( does not match the AttestBwrap( substring, so the
		// legacy v1.0 probe keeps its own allow-list.
		if strings.Contains(src, "AttestBwrap(") && !allowedAttest[name] {
			t.Errorf("%s must not call AttestBwrap( directly; route through ResolveConfinementRank", name)
		}
		for _, builder := range []string{"BuildBwrapROStageArgv(", "AttestBwrapRO("} {
			if strings.Contains(src, builder) && !allowedRankFiles[name] {
				t.Errorf("%s must not call %s directly; route through ResolveConfinementRank", name, builder)
			}
		}
		if strings.Contains(src, "directIsolatedArgv(") {
			t.Errorf("%s references the deleted builder directIsolatedArgv(", name)
		}
	}

	directSrc, err := os.ReadFile("direct.go")
	if err != nil {
		t.Fatalf("read direct.go: %v", err)
	}
	direct := string(directSrc)
	for _, banned := range []string{"AttestBwrap(", "BuildBwrapROStageArgv(", "AttestBwrapRO("} {
		if strings.Contains(direct, banned) {
			t.Errorf("direct.go must route every stage through the single resolved ConfinementSpec (found %s)", banned)
		}
	}
	if !strings.Contains(direct, "ResolveConfinementRank") {
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
