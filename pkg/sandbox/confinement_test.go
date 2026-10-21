package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// TestResolveConfinementRankDetachesCallerCancel proves the process-lifetime
// rank probe is immune to the first caller's context cancellation: the
// detached probe context keeps its own deadline and never inherits a cancelled
// parent, so one aborted request cannot poison the memoized decision for the
// whole server process (11-REVIEW WR-01 / T-261021-03).
func TestResolveConfinementRankDetachesCallerCancel(t *testing.T) {
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	if caller.Err() == nil {
		t.Fatal("test setup: caller context must be cancelled")
	}

	probe, stop := detachedProbeContext(caller)
	defer stop()
	if err := probe.Err(); err != nil {
		t.Fatalf("detached probe context must not inherit caller cancellation, got %v", err)
	}
	select {
	case <-probe.Done():
		t.Fatal("detached probe context must remain live after caller cancellation")
	default:
	}
	deadline, ok := probe.Deadline()
	if !ok {
		t.Fatal("detached probe context must carry a bounded deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("detached probe deadline = %v away, want within (0, 30s]", remaining)
	}

	// A live caller context is still bounded by the same independent deadline.
	probe2, stop2 := detachedProbeContext(context.Background())
	defer stop2()
	if _, ok := probe2.Deadline(); !ok {
		t.Fatal("live caller: detached probe context must still carry a deadline")
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

// TestLandlockRankWrapArgvContract pins the exact landlock argv contract:
// helper landlock-run --ro / --rw <workspace> --rw <cache> --rw <tmp>
// --rw <home> -- <cmd...>. The helper must receive exactly the roots the
// server pinned, and an unresolvable helper or root must refuse the stage.
func TestLandlockRankWrapArgvContract(t *testing.T) {
	paths := provisionTestJobPaths(t)
	helper := "/opt/pr-review-sandbox-helper"
	command := []string{"go", "build", "./..."}

	argv, err := landlockConfinementSpec().WrapArgv(command, paths, WrapOptions{HelperPath: helper})
	if err != nil {
		t.Fatalf("landlock wrap: %v", err)
	}
	want := []string{
		helper, "landlock-run", "--ro", "/",
		"--rw", paths.WorkspaceDir,
		"--rw", paths.CacheDir,
		"--rw", paths.TmpDir,
		"--rw", paths.HomeDir,
		"--", "go", "build", "./...",
	}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("landlock argv = %q, want %q", argv, want)
	}
	tail := argv[len(argv)-len(command):]
	for i := range command {
		if tail[i] != command[i] {
			t.Fatalf("command tail was rewritten: got %q, want %q", tail, command)
		}
	}
	if strings.Contains(strings.Join(argv, " "), "bwrap") {
		t.Errorf("landlock argv must not stack bwrap: %q", argv)
	}

	t.Run("unresolvable-helper-refuses", func(t *testing.T) {
		t.Setenv(SandboxHelperPathEnv, filepath.Join(t.TempDir(), "missing-helper"))
		got, err := landlockConfinementSpec().WrapArgv(command, paths, WrapOptions{})
		if !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
		if got != nil {
			t.Fatalf("failed wrap must return a nil argv, got %q", got)
		}
	})

	t.Run("missing-root-refuses", func(t *testing.T) {
		broken := paths
		broken.WorkspaceDir = ""
		got, err := landlockConfinementSpec().WrapArgv(command, broken, WrapOptions{HelperPath: helper})
		if !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
		if got != nil {
			t.Fatalf("failed wrap must return a nil argv, got %q", got)
		}
	})
}

// TestLandlockStage0WrapAndDirectPrepare proves stage-0 dependency preparation
// is rank-agnostic: the landlock WrapArgv rewrites the bwrap-only in-sandbox
// placeholder paths to host paths, grants --rw on the per-job gateway socket
// directory, and directPrepare then executes through runDirectStage (never
// RunBwrapStage) with the scrubbed job-private environment (11-REVIEW CR-01).
func TestLandlockStage0WrapAndDirectPrepare(t *testing.T) {
	paths := provisionTestJobPaths(t)
	sockDir := filepath.Join(paths.Root, "control-test")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatalf("mkdir control dir: %v", err)
	}
	sockPath := filepath.Join(sockDir, "gateway.sock")
	helper := filepath.Join(t.TempDir(), HelperBinaryName)

	stage0 := []string{BwrapROHelperPath, "prepare", "-socket", BwrapGatewaySocketPath, "-work", paths.WorkspaceDir}
	argv, err := landlockConfinementSpec().WrapArgv(stage0, paths, WrapOptions{GatewaySocketPath: sockPath, HelperPath: helper})
	if err != nil {
		t.Fatalf("landlock stage-0 wrap: %v", err)
	}
	want := []string{
		helper, "landlock-run", "--ro", "/",
		"--rw", paths.WorkspaceDir,
		"--rw", paths.CacheDir,
		"--rw", paths.TmpDir,
		"--rw", paths.HomeDir,
		"--rw", sockDir,
		"--", helper, "prepare", "-socket", sockPath, "-work", paths.WorkspaceDir,
	}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("stage-0 landlock argv = %q, want %q", argv, want)
	}
	if !containsTriple(argv, "--rw", sockDir, "--") {
		t.Errorf("stage-0 argv must grant --rw on the gateway socket directory %q: %q", sockDir, argv)
	}
	if strings.Contains(strings.Join(argv, " "), BwrapROHelperPath) ||
		strings.Contains(strings.Join(argv, " "), BwrapGatewaySocketPath) {
		t.Errorf("stage-0 argv must rewrite the bwrap-only placeholder paths: %q", argv)
	}

	t.Run("bad-gateway-socket-refuses", func(t *testing.T) {
		for _, bad := range []string{"relative/gateway.sock", "/gateway.sock"} {
			got, err := landlockConfinementSpec().WrapArgv(stage0, paths, WrapOptions{GatewaySocketPath: bad, HelperPath: helper})
			if !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("socket %q: want ErrConfinementUnavailable, got %v", bad, err)
			}
			if got != nil {
				t.Fatalf("socket %q: failed wrap must return nil argv, got %q", bad, got)
			}
		}
	})

	// End-to-end: a fake helper records its argv and environment, then exits 0.
	argvDump := filepath.Join(t.TempDir(), "argv.txt")
	envDump := filepath.Join(t.TempDir(), "env.txt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$0\" \"$@\" > %q\nenv > %q\nexit 0\n", argvDump, envDump)
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	t.Setenv(SandboxHelperPathEnv, helper)
	t.Setenv("GITHUB_TOKEN", "stage0-sentinel-secret")
	t.Setenv("GOTOOLCHAIN", "auto")

	res, err := directPrepare(context.Background(), 30*time.Second, paths, landlockConfinementSpec())
	if err != nil {
		t.Fatalf("directPrepare under landlock: %v", err)
	}
	if res == nil || !res.Passed {
		t.Fatalf("stage-0 must succeed through runDirectStage, got %+v", res)
	}
	rawArgv, err := os.ReadFile(argvDump)
	if err != nil {
		t.Fatalf("read helper argv: %v", err)
	}
	gotArgv := strings.Split(strings.TrimSuffix(string(rawArgv), "\n"), "\n")
	if len(gotArgv) < 6 || gotArgv[0] != helper {
		t.Fatalf("helper argv = %q, want helper %q first", gotArgv, helper)
	}
	if gotArgv[1] != "landlock-run" {
		t.Fatalf("helper argv = %q, want landlock-run subcommand", gotArgv)
	}
	prepareIndex := -1
	socketValue := ""
	for i := 0; i+1 < len(gotArgv); i++ {
		if gotArgv[i] == "prepare" {
			prepareIndex = i
		}
		if gotArgv[i] == "-socket" {
			socketValue = gotArgv[i+1]
		}
	}
	if prepareIndex < 0 {
		t.Fatalf("helper argv = %q, want the prepare subcommand", gotArgv)
	}
	if socketValue == "" || socketValue == BwrapGatewaySocketPath {
		t.Fatalf("-socket must be rewritten to a host path, got %q", socketValue)
	}
	if !strings.HasPrefix(socketValue, paths.Root+string(os.PathSeparator)+"control-") {
		t.Errorf("-socket %q must live under the job root control dir", socketValue)
	}

	rawEnv, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatalf("read helper env: %v", err)
	}
	envText := string(rawEnv)
	if strings.Contains(envText, "GITHUB_TOKEN=") {
		t.Errorf("secret-bearing env leaked into the stage-0 environment:\n%s", envText)
	}
	if strings.Contains(envText, "GOTOOLCHAIN=") {
		t.Errorf("inherited GOTOOLCHAIN leaked into the stage-0 environment:\n%s", envText)
	}
	if !strings.Contains(envText, "HOME="+paths.HomeDir+"\n") {
		t.Errorf("stage-0 env must pin HOME to the job-private home %q:\n%s", paths.HomeDir, envText)
	}
}

// TestLandlockProbeFakeHelper drives the real probe shape against an executable
// fake helper that exits with a chosen code, proving the 70–76 mapping at probe
// time: 70/71/72 are rank-unavailable, 74 can never select the rank, and the
// probe argv is exactly the production contract.
func TestLandlockProbeFakeHelper(t *testing.T) {
	tmp := t.TempDir()
	helperPath := filepath.Join(tmp, "pr-review-sandbox-helper")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit \"${FAKE_LANDLOCK_EXIT:-0}\"\n"), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	scratch := filepath.Join(tmp, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("scratch: %v", err)
	}

	var captured []string
	runFake := func(code int) func(context.Context, []string) (int, string, error) {
		return func(ctx context.Context, argv []string) (int, string, error) {
			captured = append([]string{}, argv...)
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), "FAKE_LANDLOCK_EXIT="+strconv.Itoa(code))
			stderr := NewLimitedCollector(DefaultMaxRetainedBytes)
			cmd.Stderr = stderr
			exit := 0
			err := cmd.Run()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exit = exitErr.ExitCode()
					err = nil
				}
			}
			return exit, stderr.Formatted("probe"), err
		}
	}
	base := landlockProbeDeps{
		resolve:   func() (string, error) { return helperPath, nil },
		mkScratch: func() (string, func(), error) { return scratch, func() {}, nil },
		// The ABI query runs first and must succeed here, otherwise the real
		// kernel would short-circuit the fake helper on this host.
		queryABI: func() (int, error) { return 7, nil },
	}

	t.Run("exit-0-selects-the-rank", func(t *testing.T) {
		deps := base
		deps.run = runFake(0)
		if err := probeLandlockRankWith(context.Background(), deps); err != nil {
			t.Fatalf("exit 0 must select the rank, got %v", err)
		}
		want := []string{helperPath, "landlock-run", "--ro", "/", "--rw", scratch, "--", "/bin/true"}
		if strings.Join(captured, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("probe argv = %q, want %q", captured, want)
		}
	})

	for _, code := range []int{HelperExitUnavailable, HelperExitRuleset, HelperExitDomainEstablish} {
		code := code
		t.Run(fmt.Sprintf("exit-%d-is-unavailable", code), func(t *testing.T) {
			deps := base
			deps.run = runFake(code)
			err := probeLandlockRankWith(context.Background(), deps)
			if !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("exit %d: want ErrConfinementUnavailable, got %v", code, err)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(code)) {
				t.Errorf("exit %d: error must name the code: %v", code, err)
			}
		})
	}

	for _, code := range []int{HelperExitPositiveControl, HelperExitUsage, HelperExitExec} {
		code := code
		t.Run(fmt.Sprintf("exit-%d-is-misconfiguration", code), func(t *testing.T) {
			deps := base
			deps.run = runFake(code)
			if err := probeLandlockRankWith(context.Background(), deps); !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("exit %d: want ErrConfinementUnavailable, got %v", code, err)
			}
		})
	}

	t.Run("exit-74-never-selects", func(t *testing.T) {
		deps := base
		deps.run = runFake(HelperExitNegativeControl)
		err := probeLandlockRankWith(context.Background(), deps)
		if err == nil {
			t.Fatal("an unenforced domain (exit 74) must never select the rank")
		}
		if !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
		if !strings.Contains(err.Error(), "read-only root was writable") {
			t.Errorf("error must state the denial: %v", err)
		}
	})

	for _, reason := range []LandlockProbeReason{
		LandlockReasonUnsupported,
		LandlockReasonDisabled,
		LandlockReasonBlocked,
		LandlockReasonInvalid,
		LandlockReasonTooOld,
	} {
		reason := reason
		t.Run("abi-query-"+string(reason)+"-never-spawns", func(t *testing.T) {
			deps := base
			deps.queryABI = func() (int, error) {
				return 0, newLandlockProbeError(reason, 95, 0, "")
			}
			deps.run = func(context.Context, []string) (int, string, error) {
				t.Fatal("the helper must not be spawned when the ABI query fails")
				return 0, "", nil
			}
			deps.resolve = func() (string, error) {
				t.Fatal("the helper must not be resolved when the ABI query fails")
				return "", nil
			}
			err := probeLandlockRankWith(context.Background(), deps)
			if !errors.Is(err, ErrConfinementUnavailable) {
				t.Fatalf("want ErrConfinementUnavailable, got %v", err)
			}
			if got, ok := LandlockProbeErrorReason(err); !ok || got != reason {
				t.Fatalf("typed reason = (%q, %v), want (%q, true); err=%v", got, ok, reason, err)
			}
		})
	}

	t.Run("too-old-abi-never-spawns", func(t *testing.T) {
		deps := base
		deps.queryABI = func() (int, error) { return 1, nil }
		deps.run = func(context.Context, []string) (int, string, error) {
			t.Fatal("the helper must not be spawned for a too-old ABI")
			return 0, "", nil
		}
		deps.resolve = func() (string, error) {
			t.Fatal("the helper must not be resolved for a too-old ABI")
			return "", nil
		}
		err := probeLandlockRankWith(context.Background(), deps)
		if got, ok := LandlockProbeErrorReason(err); !ok || got != LandlockReasonTooOld {
			t.Fatalf("typed reason = (%q, %v), want (%q, true); err=%v", got, ok, LandlockReasonTooOld, err)
		}
	})

	t.Run("resolve-failure", func(t *testing.T) {
		deps := base
		deps.resolve = func() (string, error) { return "", errors.New("no helper") }
		if err := probeLandlockRankWith(context.Background(), deps); !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
	})

	t.Run("runner-error", func(t *testing.T) {
		deps := base
		deps.run = func(context.Context, []string) (int, string, error) {
			return -1, "", errors.New("spawn failed")
		}
		if err := probeLandlockRankWith(context.Background(), deps); !errors.Is(err, ErrConfinementUnavailable) {
			t.Fatalf("want ErrConfinementUnavailable, got %v", err)
		}
	})
}

// TestHelperExitMapsToDeniedOrUnavailableAtExecution proves the split: exit 74
// (the negative control) becomes StatusConfinementDenied, while 70–73/75–76
// become StatusUnavailable, and none can ever become a build/test verdict
// (T-10-06, CONFINE-04).
func TestHelperExitMapsToDeniedOrUnavailableAtExecution(t *testing.T) {
	// Exit 74 is a proven security denial, never an unavailable rank.
	{
		report := &VerificationReport{Status: StatusBuildFailed, Reason: "stale"}
		res := &StageResult{ExitCode: HelperExitNegativeControl, Passed: false}
		if !applyHelperExitToReport(report, RankLandlock, "install", res) {
			t.Fatal("exit 74: expected the mapping to claim the result")
		}
		if report.Status != StatusConfinementDenied {
			t.Fatalf("exit 74: status = %q, want %q", report.Status, StatusConfinementDenied)
		}
		if report.FailedStage != "install" {
			t.Errorf("exit 74: FailedStage = %q, want install", report.FailedStage)
		}
		if !strings.Contains(report.Reason, "74") {
			t.Errorf("exit 74: reason must name the code: %q", report.Reason)
		}
		if !strings.Contains(report.Reason, "not enforcing") {
			t.Errorf("exit 74: reason must name the non-enforcing domain: %q", report.Reason)
		}
		if report.ConfinementDeniedPath != "" {
			t.Errorf("exit 74: the pre-exec proof has no path, got %q", report.ConfinementDeniedPath)
		}
		if !strings.Contains(report.Summary, "CONFINEMENT-DENIED") {
			t.Errorf("exit 74: summary must carry the denial label: %q", report.Summary)
		}
		if report.Status == StatusUnavailable || report.Status == StatusBuildFailed || report.Status == StatusTestFailed {
			t.Errorf("exit 74: a proven denial must never be unavailable or a build/test failure: %q", report.Status)
		}
	}

	// 70–73/75–76 stay unavailable.
	for _, code := range []int{70, 71, 72, 73, 75, 76} {
		report := &VerificationReport{Status: StatusBuildFailed, Reason: "stale"}
		res := &StageResult{ExitCode: code, Passed: false}
		if !applyHelperExitToReport(report, RankLandlock, "test", res) {
			t.Fatalf("exit %d: expected the mapping to claim the result", code)
		}
		if report.Status != StatusUnavailable {
			t.Fatalf("exit %d: status = %q, want %q", code, report.Status, StatusUnavailable)
		}
		if report.FailedStage != "test" {
			t.Errorf("exit %d: FailedStage = %q, want test", code, report.FailedStage)
		}
		if !strings.Contains(report.Reason, strconv.Itoa(code)) {
			t.Errorf("exit %d: reason must name the code: %q", code, report.Reason)
		}
		if report.Status == StatusBuildFailed || report.Status == StatusTestFailed {
			t.Errorf("exit %d: a helper exit must never be a build/test failure", code)
		}
	}

	for _, tc := range []struct {
		name   string
		report *VerificationReport
		rank   ConfinementRank
		res    *StageResult
	}{
		{"nil-report", nil, RankLandlock, &StageResult{ExitCode: 70}},
		{"nil-result", &VerificationReport{}, RankLandlock, nil},
		{"passing-stage", &VerificationReport{}, RankLandlock, &StageResult{ExitCode: 0, Passed: true}},
		{"bwrap-rank", &VerificationReport{}, RankBwrapRO, &StageResult{ExitCode: 70}},
		{"non-contract-code", &VerificationReport{}, RankLandlock, &StageResult{ExitCode: 1}},
		{"timeout-code", &VerificationReport{}, RankLandlock, &StageResult{ExitCode: 124}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if applyHelperExitToReport(tc.report, tc.rank, "build", tc.res) {
				t.Fatal("mapping must not claim this result")
			}
		})
	}
}

// TestVerifyDirGatedExit74IsConfinementDenied proves the end-to-end path: the
// landlock rank runs a fake helper that exits 74, and the run ends in
// StatusConfinementDenied with ConfinementRank "landlock" — never a build or
// test verdict (CONFINE-04).
func TestVerifyDirGatedExit74IsConfinementDenied(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent on PATH")
	}
	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/exit74\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	helperPath := filepath.Join(t.TempDir(), "pr-review-sandbox-helper")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 74\n"), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	t.Setenv(SandboxHelperPathEnv, helperPath)

	setConfinementResolverOverrideForTest(func(context.Context) (ConfinementSpec, error) {
		return landlockConfinementSpec(), nil
	})
	defer resetConfinementRankForTest()

	report, err := newTestDirectRunner(t, 30*time.Second).VerifyDirGated(context.Background(), dir, directTestSHA, false)
	if err != nil {
		t.Fatalf("VerifyDirGated: %v", err)
	}
	if report.Status != StatusConfinementDenied {
		t.Fatalf("status = %q, want %q (reason=%q)", report.Status, StatusConfinementDenied, report.Reason)
	}
	if report.ConfinementRank != string(RankLandlock) {
		t.Fatalf("rank = %q, want %q", report.ConfinementRank, RankLandlock)
	}
	if len(report.Results) != 1 {
		t.Fatalf("results = %d, want exactly one (the target never ran)", len(report.Results))
	}
	if report.Results[0].ExitCode != HelperExitNegativeControl {
		t.Fatalf("exit = %d, want %d", report.Results[0].ExitCode, HelperExitNegativeControl)
	}
}

// TestOnHostLandlockRankUnavailableReason records the real host state and
// never fails on the expected EOPNOTSUPP: on this LXC Landlock is compiled in
// but disabled at boot, so a recorded Disabled reason is a pass (T-11-03).
// It only ever performs the read-only version query; no domain is applied.
func TestOnHostLandlockRankUnavailableReason(t *testing.T) {
	abi, err := queryLandlockABI()
	if err != nil {
		reason, ok := LandlockProbeErrorReason(err)
		if !ok {
			t.Fatalf("the on-host ABI error must be typed: %v", err)
		}
		switch reason {
		case LandlockReasonUnsupported,
			LandlockReasonDisabled,
			LandlockReasonBlocked,
			LandlockReasonInvalid,
			LandlockReasonTooOld,
			LandlockReasonNotLinux:
		default:
			t.Fatalf("unexpected on-host Landlock reason %q", reason)
		}
		msg := LandlockProbeReasonMessage(reason, "")
		if strings.TrimSpace(msg) == "" {
			t.Fatal("the reason message must be non-empty and actionable")
		}
		t.Logf("on-host Landlock ABI query: abi=%d reason=%s err=%v", abi, reason, err)
		return
	}
	if abi < MinLandlockABI {
		t.Fatalf("a successful query must report a supported ABI, got %d", abi)
	}
	t.Logf("on-host Landlock ABI query succeeded: abi=%d", abi)
}

// serverABIProbeIsQueryOnly reports whether a source file's use of the
// create_ruleset syscall is the read-only version query: it must reference the
// version cookie and none of the applier tokens.
func serverABIProbeIsQueryOnly(src string) bool {
	if !strings.Contains(src, "LANDLOCK_CREATE_RULESET_VERSION") {
		return false
	}
	for _, banned := range []string{
		"LANDLOCK_RULE_PATH_BENEATH",
		"SYS_LANDLOCK_ADD_RULE",
		"SYS_LANDLOCK_RESTRICT_SELF",
		"PR_SET_NO_NEW_PRIVS",
		"applyLandlockDomain",
	} {
		if strings.Contains(src, banned) {
			return false
		}
	}
	return true
}

// TestServerABIProbeIsQueryOnly is the structural proof that the server-side
// ABI probe only ever asks for the version cookie and never builds a ruleset or
// applies a domain (T-11-01).
func TestServerABIProbeIsQueryOnly(t *testing.T) {
	raw, err := os.ReadFile("landlock_abi_linux.go")
	if err != nil {
		t.Fatalf("read landlock_abi_linux.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "LANDLOCK_CREATE_RULESET_VERSION") {
		t.Error("landlock_abi_linux.go must query LANDLOCK_CREATE_RULESET_VERSION")
	}
	for _, banned := range []string{
		"LANDLOCK_RULE_PATH_BENEATH",
		"SYS_LANDLOCK_ADD_RULE",
		"SYS_LANDLOCK_RESTRICT_SELF",
		"PR_SET_NO_NEW_PRIVS",
		"applyLandlockDomain",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("landlock_abi_linux.go must not contain %s", banned)
		}
	}
	if got := strings.Count(src, "SYS_LANDLOCK_CREATE_RULESET"); got != 1 {
		t.Errorf("landlock_abi_linux.go must have exactly one SYS_LANDLOCK_CREATE_RULESET call site, found %d", got)
	}
	if !strings.Contains(src, "unix.LANDLOCK_CREATE_RULESET_VERSION") {
		t.Error("the single call site must pass the version cookie")
	}
}

// TestNoServerCallsLandlockApplier is the structural proof that the server
// never calls restrict_self (T-10-01): the applier tokens may appear only in
// non-test files under cmd/pr-review-sandbox-helper/.
func TestNoServerCallsLandlockApplier(t *testing.T) {
	root := filepath.Join("..", "..")
	allowedPrefix := filepath.Join("cmd", "pr-review-sandbox-helper") + string(filepath.Separator)
	tokens := []string{
		"SYS_LANDLOCK_RESTRICT_SELF",
		"SYS_LANDLOCK_ADD_RULE",
		"SYS_LANDLOCK_CREATE_RULESET",
		"PR_SET_NO_NEW_PRIVS",
		"applyLandlockDomain",
	}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".planning", "node_modules", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		if strings.HasPrefix(rel, allowedPrefix) {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		for _, token := range tokens {
			if !strings.Contains(string(raw), token) {
				continue
			}
			// The read-only ABI version query is permitted outside the
			// helper, but only when the same file is provably query-only.
			if token == "SYS_LANDLOCK_CREATE_RULESET" && serverABIProbeIsQueryOnly(string(raw)) {
				continue
			}
			t.Errorf("%s references %s; only cmd/pr-review-sandbox-helper/ may contain the applier", rel, token)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// TestGoreleaserHelperBuildIsLinuxOnly is the reproducible config proof (no
// goreleaser binary is installed): the helper must stay a linux-only,
// CGO-free amd64+arm64 artifact that ships in the release archives.
func TestGoreleaserHelperBuildIsLinuxOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	src := string(raw)

	const marker = "id: pr-review-sandbox-helper"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf(".goreleaser.yaml must declare a %s build", marker)
	}
	rest := src[start:]
	// The block ends at the next build id or the next top-level key.
	next := len(rest)
	for _, sep := range []string{"\n  - id:", "\narchives:"} {
		if idx := strings.Index(rest[len(marker):], sep); idx >= 0 {
			if pos := len(marker) + idx; pos < next {
				next = pos
			}
		}
	}
	block := rest[:next]

	for _, want := range []string{
		"main: ./cmd/pr-review-sandbox-helper",
		"CGO_ENABLED=0",
		"goos: [linux]",
		"goarch: [amd64, arm64]",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("pr-review-sandbox-helper build block must contain %q:\n%s", want, block)
		}
	}
	for _, unwanted := range []string{"darwin", "windows"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("pr-review-sandbox-helper build block must not target %s:\n%s", unwanted, block)
		}
	}

	archivesStart := strings.Index(src, "archives:")
	if archivesStart < 0 {
		t.Fatal(".goreleaser.yaml must declare archives")
	}
	archivesSection := src[archivesStart:]
	if idx := strings.Index(archivesSection, "\nchecksum:"); idx >= 0 {
		archivesSection = archivesSection[:idx]
	}
	if !strings.Contains(archivesSection, "pr-review-sandbox-helper") {
		t.Errorf("archives.ids must include pr-review-sandbox-helper:\n%s", archivesSection)
	}
}

// TestConfinementRankNetworkGuarantee pins the per-rank network claim against
// the code. docs/SANDBOX.md F-17 and README.md both state that `bwrap-ro`
// removes the network while `landlock` does not; this asserts the
// NetworkIsolated field behind that wording directly from the rank specs so the
// documented claim cannot drift from the implementation. It uses only the
// package-internal spec constructors — no kernel probe, no
// ResolveConfinementRank, no syscalls — so it is hermetic on every platform.
func TestConfinementRankNetworkGuarantee(t *testing.T) {
	bwrap := bwrapROConfinementSpec()
	if bwrap.Rank != RankBwrapRO {
		t.Errorf("bwrapROConfinementSpec().Rank = %q, want %q", bwrap.Rank, RankBwrapRO)
	}
	if !bwrap.NetworkIsolated {
		t.Error("bwrapROConfinementSpec().NetworkIsolated must be true: the bwrap-ro rank retains --unshare-net (docs/SANDBOX.md F-17)")
	}

	landlock := landlockConfinementSpec()
	if landlock.Rank != RankLandlock {
		t.Errorf("landlockConfinementSpec().Rank = %q, want %q", landlock.Rank, RankLandlock)
	}
	if landlock.NetworkIsolated {
		t.Error("landlockConfinementSpec().NetworkIsolated must be false: the landlock rank removes no network interface (docs/SANDBOX.md F-17)")
	}

	if bwrap.NetworkIsolated == landlock.NetworkIsolated {
		t.Error("the two ranks must not report the same network posture, or the per-rank F-17 claim is unprovable")
	}
}
