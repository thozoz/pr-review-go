package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// containsPair reports whether argv contains the exact ordered pair a,b.
func containsPair(argv []string, a, b string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b {
			return true
		}
	}
	return false
}

// countTriple counts exact ordered triples a,b,c in argv.
func countTriple(argv []string, a, b, c string) int {
	n := 0
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b && argv[i+2] == c {
			n++
		}
	}
	return n
}

// setenvPairs extracts the effective --setenv key→value map from a bwrap argv.
func setenvPairs(argv []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--setenv" {
			m[argv[i+1]] = argv[i+2]
		}
	}
	return m
}

// skipNonLinuxBwrapRO skips argv-shape tests on hosts where the rank's Linux
// mount isolation is refused by construction.
func skipNonLinuxBwrapRO(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bwrap-ro argv requires Linux mount isolation")
	}
}

// TestBuildBwrapROStageArgvShape pins the read-only-host flag contract of
// Phase 9 success criterion 1 and T-09-07: a read-only root, explicit /dev,
// /proc and private /tmp carve-outs, same-path job binds, a pinned writable
// cwd, retained network removal, and no read-write root or device bind.
func TestBuildBwrapROStageArgvShape(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	paths := provisionTestJobPaths(t)
	command := []string{"go", "build", "./..."}

	argv, err := BuildBwrapROStageArgv(command, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("BuildBwrapROStageArgv: %v", err)
	}
	if len(argv) == 0 || argv[0] != "bwrap" {
		t.Fatalf("argv must start with bwrap, got %q", argv)
	}

	// The exact five unshare flags, including retained network removal.
	for _, flag := range []string{"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts"} {
		if !containsElement(argv, flag) {
			t.Errorf("argv must carry %s: %q", flag, argv)
		}
	}
	for _, flag := range []string{"--die-with-parent", "--new-session", "--clearenv"} {
		if !containsElement(argv, flag) {
			t.Errorf("argv must carry %s: %q", flag, argv)
		}
	}

	// The read-only host root and the three explicit carve-outs.
	if !containsTriple(argv, "--ro-bind", "/", "/") {
		t.Errorf("argv must bind the host root read-only with (--ro-bind / /): %q", argv)
	}
	if !containsPair(argv, "--dev", "/dev") {
		t.Errorf("argv must carve out a writable /dev: %q", argv)
	}
	if !containsPair(argv, "--proc", "/proc") {
		t.Errorf("argv must carve out /proc: %q", argv)
	}
	if !containsPair(argv, "--tmpfs", "/tmp") {
		t.Errorf("argv must carve out a private /tmp: %q", argv)
	}
	if !containsTriple(argv, "--chdir", paths.WorkspaceDir, "--clearenv") {
		t.Errorf("argv must pin --chdir into the writable workspace directly before --clearenv: %q", argv)
	}

	// No read-write root bind and no device bind anywhere.
	if containsElement(argv, "--unshare-all") {
		t.Errorf("argv must not use --unshare-all (the explicit flag set is the contract): %q", argv)
	}
	if containsElement(argv, "--dev-bind") {
		t.Errorf("argv must never use --dev-bind: %q", argv)
	}
	if containsTriple(argv, "--bind", "/", "/") {
		t.Errorf("argv must never read-write bind the host root: %q", argv)
	}
	// Every --ro-bind whose source is a job path must be same-path (no remap).
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] != "--ro-bind" {
			continue
		}
		for _, p := range []string{paths.WorkspaceDir, paths.TmpDir, paths.HomeDir, paths.CacheDir} {
			if argv[i+1] == p && argv[i+2] != p {
				t.Errorf("job path %q must not be remapped by --ro-bind onto %q", p, argv[i+2])
			}
		}
	}

	// The command survives intact as the argv tail, immediately after "--".
	sep := -1
	for i, a := range argv {
		if a == "--" {
			sep = i
		}
	}
	if sep < 0 {
		t.Fatalf("argv must carry the -- command separator: %q", argv)
	}
	if sep != len(argv)-1-len(command) {
		t.Fatalf("-- must sit immediately before the command, got index %d in %q", sep, argv)
	}
	if !reflect.DeepEqual(argv[sep+1:], command) {
		t.Fatalf("command tail = %q, want %q", argv[sep+1:], command)
	}
}

// containsElement reports whether argv contains s as a standalone element.
func containsElement(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// TestBuildBwrapROArgvBindsJobPathsAtSamePath proves each of the job's four
// writable roots is bound exactly once, source equal to destination, so the
// job-private env pins name paths that are writable inside the sandbox.
func TestBuildBwrapROArgvBindsJobPathsAtSamePath(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	paths := provisionTestJobPaths(t)

	argv, err := BuildBwrapROStageArgv([]string{"go", "test", "./..."}, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("BuildBwrapROStageArgv: %v", err)
	}
	for _, p := range []string{paths.WorkspaceDir, paths.TmpDir, paths.HomeDir, paths.CacheDir} {
		if n := countTriple(argv, "--bind", p, p); n != 1 {
			t.Errorf("expected exactly one same-path --bind for %q, got %d in %q", p, n, argv)
		}
		// No other bind may target that path.
		for i := 0; i+2 < len(argv); i++ {
			if (argv[i] == "--bind" || argv[i] == "--ro-bind") && argv[i+2] == p && argv[i+1] != p {
				t.Errorf("path %q must not be a bind destination for source %q: %q", p, argv[i+1], argv)
			}
		}
	}
}

// TestBwrapROEnvPinsMatchStageEnv is the no-drift invariant of T-09-06: the
// wrapper's --setenv root block and buildDirectStageEnv must emit exactly the
// same key→value map for every directRootEnvPins key.
func TestBwrapROEnvPinsMatchStageEnv(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	paths := provisionTestJobPaths(t)
	// Force a non-job value for a pinned key: it must be dropped and replaced.
	t.Setenv("HOME", "/server/HOME")

	argv, err := BuildBwrapROStageArgv([]string{"go", "build", "./..."}, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("BuildBwrapROStageArgv: %v", err)
	}
	want := map[string]string{}
	for _, pin := range directRootEnvPins(paths) {
		want[pin[0]] = pin[1]
	}
	if len(want) != len(directRootEnvKeys) {
		t.Fatalf("directRootEnvPins emitted %d keys, directRootEnvKeys lists %d", len(want), len(directRootEnvKeys))
	}

	argvPins := setenvPairs(argv)
	for key, value := range want {
		got, ok := argvPins[key]
		if !ok {
			t.Errorf("argv is missing --setenv %s: %q", key, argv)
			continue
		}
		if got != value {
			t.Errorf("--setenv %s = %q, want %q", key, got, value)
		}
	}

	envPins := map[string]string{}
	for _, kv := range buildDirectStageEnv(nil, &paths) {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		if _, isRootPin := want[kv[:i]]; isRootPin {
			envPins[kv[:i]] = kv[i+1:]
		}
	}
	if !reflect.DeepEqual(want, envPins) {
		t.Fatalf("wrapper env pins drifted from buildDirectStageEnv pins:\nwrapper=%v\nstage env=%v", want, envPins)
	}
}

// TestBwrapROStage0BindsRuntimeArtifacts proves stage-0 gains exactly the two
// read-only runtime binds into the private tmpfs, and stage-2 stays pure
// (T-09-05).
func TestBwrapROStage0BindsRuntimeArtifacts(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	paths := provisionTestJobPaths(t)

	opts := WrapOptions{GatewaySocketPath: "/host/ctl/gw.sock", HelperPath: "/host/ctl/helper"}
	stage0, err := BuildBwrapROStageArgv([]string{BwrapROHelperPath, "prepare"}, paths, opts)
	if err != nil {
		t.Fatalf("stage-0 wrap: %v", err)
	}
	if !containsTriple(stage0, "--ro-bind", opts.GatewaySocketPath, BwrapGatewaySocketPath) {
		t.Errorf("stage-0 argv must bind the gateway socket read-only into %q: %q", BwrapGatewaySocketPath, stage0)
	}
	if !containsTriple(stage0, "--ro-bind", opts.HelperPath, BwrapROHelperPath) {
		t.Errorf("stage-0 argv must bind the helper read-only into %q: %q", BwrapROHelperPath, stage0)
	}

	stage2, err := BuildBwrapROStageArgv([]string{"go", "build", "./..."}, paths, WrapOptions{})
	if err != nil {
		t.Fatalf("stage-2 wrap: %v", err)
	}
	for _, forbidden := range []string{BwrapGatewaySocketPath, BwrapROHelperPath, opts.GatewaySocketPath, opts.HelperPath, "/host/ctl"} {
		if strings.Contains(strings.Join(stage2, " "), forbidden) {
			t.Errorf("stage-2 argv must not contain %q: %q", forbidden, stage2)
		}
	}
}

// TestBuildBwrapROArgvRefusesHostRoot proves a job path of "/" is refused
// before anything is emitted.
func TestBuildBwrapROArgvRefusesHostRoot(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	base := provisionTestJobPaths(t)
	fields := []struct {
		name string
		set  func(p *DirectJobPaths)
	}{
		{"WorkspaceDir", func(p *DirectJobPaths) { p.WorkspaceDir = "/" }},
		{"TmpDir", func(p *DirectJobPaths) { p.TmpDir = "/" }},
		{"HomeDir", func(p *DirectJobPaths) { p.HomeDir = "/" }},
		{"CacheDir", func(p *DirectJobPaths) { p.CacheDir = "/" }},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			paths := base
			tc.set(&paths)
			argv, err := BuildBwrapROStageArgv([]string{"go", "build", "./..."}, paths, WrapOptions{})
			if err == nil {
				t.Fatalf("host root %s must be refused, got argv %q", tc.name, argv)
			}
			if argv != nil {
				t.Fatalf("refusal must return a nil argv, got %q", argv)
			}
			if !strings.Contains(err.Error(), "host root bind refused") {
				t.Errorf("error must name the host root refusal: %v", err)
			}
		})
	}
}

// TestBuildBwrapROArgvRefusesSocketWithoutHelper proves a gateway socket
// without the trusted helper is refused.
func TestBuildBwrapROArgvRefusesSocketWithoutHelper(t *testing.T) {
	skipNonLinuxBwrapRO(t)
	paths := provisionTestJobPaths(t)
	argv, err := BuildBwrapROStageArgv(
		[]string{BwrapROHelperPath, "prepare"},
		paths,
		WrapOptions{GatewaySocketPath: "/host/ctl/gw.sock"},
	)
	if err == nil {
		t.Fatalf("socket without helper must be refused, got argv %q", argv)
	}
	if argv != nil {
		t.Fatalf("refusal must return a nil argv, got %q", argv)
	}
	if !strings.Contains(err.Error(), "trusted preparation helper unavailable") {
		t.Errorf("error must name the missing helper: %v", err)
	}
}

// TestBwrapProbeBaseAvoidsShadowedTmp proves the probe scratch base is not the
// directory the argv shadows with --tmpfs /tmp.
func TestBwrapProbeBaseAvoidsShadowedTmp(t *testing.T) {
	base := bwrapProbeBase()
	if base == "" {
		// Only legal when every candidate is unwritable; then there is no
		// shadowing question to answer.
		return
	}
	tmp := filepath.Clean(os.TempDir())
	if base == tmp || strings.HasPrefix(base, tmp+string(os.PathSeparator)) {
		t.Fatalf("probe base %q is shadowed by --tmpfs %s", base, tmp)
	}
}

// TestAttestBwrapROProbeShape proves the probe runs the production builder's
// argv (not a hand-written proxy) plus a write control (T-09-04).
func TestAttestBwrapROProbeShape(t *testing.T) {
	root := t.TempDir()
	scratch := bwrapROScratch{
		Root:  filepath.Join(root, "root"),
		Work:  filepath.Join(root, "work"),
		Tmp:   filepath.Join(root, "tmp"),
		Home:  filepath.Join(root, "home"),
		Cache: filepath.Join(root, "cache"),
	}
	var recorded [][]string
	deps := bwrapROProbeDeps{
		lookPath: func(string) (string, error) { return "/usr/bin/bwrap", nil },
		run: func(_ context.Context, argv []string) (string, error) {
			recorded = append(recorded, append([]string(nil), argv...))
			return "bubblewrap 0.12.0", nil
		},
		mkScratch: func() (bwrapROScratch, func(), error) {
			return scratch, func() {}, nil
		},
	}
	if err := attestBwrapROWith(context.Background(), deps); err != nil {
		t.Fatalf("probe must succeed with a healthy seam: %v", err)
	}

	var shape, control []string
	for _, rec := range recorded {
		if len(rec) == 2 && rec[0] == "bwrap" && rec[1] == "--version" {
			continue
		}
		if shape == nil {
			shape = rec
			continue
		}
		control = rec
	}
	if shape == nil || control == nil {
		t.Fatalf("probe must run both the shape and the write control, recorded %d argv", len(recorded))
	}

	scratchJob := scratchPaths(scratch)
	if !containsTriple(shape, "--ro-bind", "/", "/") || !containsPair(shape, "--tmpfs", "/tmp") {
		t.Fatalf("shape argv is not the production read-only-host shape: %q", shape)
	}
	for _, p := range []string{scratchJob.WorkspaceDir, scratchJob.TmpDir, scratchJob.HomeDir, scratchJob.CacheDir} {
		if countTriple(shape, "--bind", p, p) != 1 {
			t.Fatalf("shape argv must bind scratch path %q same-path exactly once: %q", p, shape)
		}
	}
	if !reflect.DeepEqual(shape[len(shape)-2:], []string{"--", "/bin/true"}) {
		t.Fatalf("shape argv must end with -- /bin/true, got %q", shape)
	}
	wantControlTail := []string{"--", "/bin/sh", "-c", bwrapROWriteControlScript, "sh", scratch.Work}
	if !reflect.DeepEqual(control[len(control)-len(wantControlTail):], wantControlTail) {
		t.Fatalf("control argv tail = %q, want %q", control[len(control)-len(wantControlTail):], wantControlTail)
	}
}

// TestAttestBwrapROFailsClosedOnWritableRoot proves an enforcement failure is
// a typed rank refusal carrying the step name, stderr and AppArmor profile.
func TestAttestBwrapROFailsClosedOnWritableRoot(t *testing.T) {
	root := t.TempDir()
	scratch := bwrapROScratch{
		Root:  filepath.Join(root, "root"),
		Work:  filepath.Join(root, "work"),
		Tmp:   filepath.Join(root, "tmp"),
		Home:  filepath.Join(root, "home"),
		Cache: filepath.Join(root, "cache"),
	}
	deps := bwrapROProbeDeps{
		lookPath: func(string) (string, error) { return "/usr/bin/bwrap", nil },
		run: func(_ context.Context, argv []string) (string, error) {
			if containsElement(argv, "-c") {
				// The write control is the enforcement proof.
				return "rank root is writable\n", errors.New("exit status 1")
			}
			return "bubblewrap 0.12.0", nil
		},
		mkScratch: func() (bwrapROScratch, func(), error) {
			return scratch, func() {}, nil
		},
	}
	err := attestBwrapROWith(context.Background(), deps)
	if err == nil {
		t.Fatal("a writable root must fail the rank probe")
	}
	if !errors.Is(err, ErrBwrapUnavailable) {
		t.Fatalf("probe failure must wrap ErrBwrapUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "rank root is writable") {
		t.Errorf("error must carry the captured stderr: %v", err)
	}
	if !strings.Contains(err.Error(), bwrapROStepWriteControl) {
		t.Errorf("error must name the failing step %q: %v", bwrapROStepWriteControl, err)
	}
	if !strings.Contains(err.Error(), "apparmor profile:") {
		t.Errorf("error must carry the AppArmor profile: %v", err)
	}
}

// TestAttestBwrapROUnavailableWithoutBwrap proves the probe fails closed when
// bwrap is not on PATH.
func TestAttestBwrapROUnavailableWithoutBwrap(t *testing.T) {
	deps := defaultBwrapROProbeDeps()
	deps.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	err := attestBwrapROWith(context.Background(), deps)
	if !errors.Is(err, ErrBwrapUnavailable) {
		t.Fatalf("missing bwrap must wrap ErrBwrapUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Errorf("error must name bwrap: %v", err)
	}
}

// TestNoReadWriteRootBindInDirectPath is the structural form of Phase 9
// success criterion 1: no non-test source file may reintroduce a read-write
// host-root bind or a device bind. The chosen literals are the exact pairs a
// reintroduction would have to write: `--dev-bind` and `"--bind", "/"` (with
// optional whitespace around the comma).
func TestNoReadWriteRootBindInDirectPath(t *testing.T) {
	rwRoot := regexp.MustCompile(`"--bind"\s*,\s*"/"`)
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
		if strings.Contains(src, "--dev-bind") {
			t.Errorf("%s reintroduces a device bind (--dev-bind)", name)
		}
		if rwRoot.MatchString(src) {
			t.Errorf("%s reintroduces a read-write host root bind", name)
		}
	}
}
