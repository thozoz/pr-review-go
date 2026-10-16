package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DirectRunner executes verification directly on the host without any
// container runtime. It is intended ONLY for pre-isolated environments
// (unprivileged LXC, Docker, Kubernetes pods, CI runners) where the server
// process itself is already sandboxed — project code runs as the service user.
//
// Safety contract (mirrors the container path):
//   - No shell execution: fixed argv whitelists, exec.Command only.
//   - Offline resolution where supported (GOPROXY=off, GOTOOLCHAIN=local,
//     CARGO_NET_OFFLINE=true) so builds use local caches or fail honestly.
//   - Each job runs in a job-private <WorkRoot>/jobs/<id>/workspace copied
//     from the snapshot; the snapshot source is never mutated. Tmp, home and
//     every toolchain cache are job-private too, resolved under the
//     configured WorkRoot/CacheRoot rather than the shared host TMPDIR.
//   - Per-stage timeout via context; output capped by LimitedCollector.
//   - Same verification gate: non-passed status means changes are never
//     committed or pushed by callers.
type DirectRunner struct {
	Timeout time.Duration
	// WorkRoot and CacheRoot are the operator-configured job-private roots
	// (ROOT-01). When either is empty the runner fails closed with
	// StatusUnavailable instead of creating directories under host /tmp.
	WorkRoot  string
	CacheRoot string
}

// NewDirectRunner creates a DirectRunner with the given per-stage timeout.
// Non-positive timeouts fall back to the 5-minute execution default.
func NewDirectRunner(timeout time.Duration) *DirectRunner {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &DirectRunner{Timeout: timeout}
}

// NewDirectRunnerWithRoots creates a DirectRunner bound to the operator's
// job-private roots.
func NewDirectRunnerWithRoots(timeout time.Duration, roots DirectRoots) *DirectRunner {
	d := NewDirectRunner(timeout)
	d.WorkRoot = roots.WorkRoot
	d.CacheRoot = roots.CacheRoot
	return d
}

// resolveJobPaths provisions this runner's job-private tree. An unconfigured
// or unsafe root returns an error before any job-named path is created.
func (d *DirectRunner) resolveJobPaths(jobID string) (DirectJobPaths, error) {
	return provisionDirectJobDirs(DirectRoots{WorkRoot: d.WorkRoot, CacheRoot: d.CacheRoot}, jobID)
}

// directJobID derives a lowercase-hex job ID from the commit SHA and the
// current time, mirroring the container slot job-ID shape in runner.go.
func directJobID(commitSHA string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", commitSHA, time.Now().UnixNano())))
	return fmt.Sprintf("%x", sum)[:12]
}

// directProject describes the verified commands for one project type.
type directProject struct {
	lang string
	// installArgs warms the dependency cache before build/test.
	// Install failure yields StatusIncomplete (could not prepare).
	installArgs []string
	buildArgs   []string // nil when the type has no separate build step
	// testOptions lists candidate test commands; the first whose binary
	// exists on PATH wins.
	testOptions [][]string
	offline     []string // extra hermetic env overrides
}

// directProjectTable maps marker files to their verification commands.
// Lockfiles take precedence over manifests so the project's own package
// manager (poetry, pnpm, yarn) is used when pinned.
func detectDirectProject(dir string) *directProject {
	if fileExists(filepath.Join(dir, "go.mod")) {
		proj := &directProject{
			lang:        "go",
			buildArgs:   []string{"go", "build", "./..."},
			testOptions: [][]string{{"go", "test", "-race", "./..."}},
			offline:     []string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOVCS=off", "GOENV=off"},
		}
		if hasUnvendoredDependencies(dir) {
			proj.installArgs = []string{"go", "mod", "download"}
		}
		return proj
	}
	if fileExists(filepath.Join(dir, "poetry.lock")) {
		return &directProject{
			lang:        "python",
			installArgs: []string{"poetry", "install"},
			testOptions: [][]string{{"poetry", "run", "pytest"}, {"pytest"}, {"python3", "-m", "pytest"}, {"python", "-m", "pytest"}, {"python3", "-m", "unittest"}, {"python", "-m", "unittest"}},
		}
	}
	if fileExists(filepath.Join(dir, "requirements.txt")) {
		return &directProject{
			lang:        "python",
			installArgs: []string{"pip", "install", "-r", "requirements.txt"},
			testOptions: [][]string{{"pytest"}, {"python3", "-m", "pytest"}, {"python", "-m", "pytest"}, {"python3", "-m", "unittest"}, {"python", "-m", "unittest"}},
		}
	}
	if fileExists(filepath.Join(dir, "pyproject.toml")) {
		return &directProject{
			lang:        "python",
			installArgs: []string{"pip", "install", "-e", "."},
			testOptions: [][]string{{"pytest"}, {"python3", "-m", "pytest"}, {"python", "-m", "pytest"}, {"python3", "-m", "unittest"}, {"python", "-m", "unittest"}},
		}
	}
	if fileExists(filepath.Join(dir, "pnpm-lock.yaml")) {
		return &directProject{
			lang:        "node",
			installArgs: []string{"pnpm", "install"},
			testOptions: [][]string{{"pnpm", "test"}},
		}
	}
	if fileExists(filepath.Join(dir, "yarn.lock")) {
		return &directProject{
			lang:        "node",
			installArgs: []string{"yarn", "install", "--frozen-lockfile"},
			testOptions: [][]string{{"yarn", "test"}},
		}
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		proj := &directProject{
			lang:        "node",
			installArgs: []string{"npm", "install"},
			testOptions: [][]string{{"npm", "test"}},
		}
		if fileExists(filepath.Join(dir, "package-lock.json")) {
			proj.installArgs = []string{"npm", "ci"}
		}
		return proj
	}
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return &directProject{
			lang:        "rust",
			installArgs: []string{"cargo", "fetch"},
			testOptions: [][]string{{"cargo", "test"}},
			offline:     []string{"CARGO_NET_OFFLINE=true"},
		}
	}
	if fileExists(filepath.Join(dir, "pom.xml")) {
		return &directProject{lang: "java", testOptions: [][]string{{"mvn", "-q", "test"}}}
	}
	if fileExists(filepath.Join(dir, "build.gradle")) || fileExists(filepath.Join(dir, "build.gradle.kts")) {
		return &directProject{lang: "java", testOptions: [][]string{{"gradle", "test"}}}
	}
	if fileExists(filepath.Join(dir, "Makefile")) || fileExists(filepath.Join(dir, "GNUmakefile")) ||
		fileExists(filepath.Join(dir, "makefile")) {
		return &directProject{lang: "make", testOptions: [][]string{{"make", "test"}}}
	}
	return nil
}

// directSecretEnvPrefixes lists secret-bearing env key prefixes that must
// never flow from the server process into stage-2 exec (T-07-06, D-12).
// A key is dropped when it equals or starts with any entry.
var directSecretEnvPrefixes = []string{"GH_TOKEN", "GITHUB_TOKEN", "GITHUB_", "LLM_", "APP_", "OPENAI_", "ANTHROPIC_"}

// directGoEnvOverrides lists Go toolchain env keys that must never be
// inherited into stage exec: the pinned offline list is authoritative, and
// neither the caller environment nor a go.env file may re-enable fetch
// (GOPROXY) or weaken verification (GOFLAGS, sumdb bypasses).
var directGoEnvOverrides = []string{"GOFLAGS", "GOPROXY", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE", "GOSUMDB"}

// directRootEnvKeys lists the toolchain root env keys that must never be
// inherited from the server process (ROOT-02, T-08-02): every one of them is
// dropped and then re-pinned to the job's private path. Dropping first matters
// because a duplicated key would let the child's first-match getenv resolve to
// the server's inherited value (T-08-03).
var directRootEnvKeys = []string{
	"HOME", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH", "GOMODCACHE",
	"XDG_CACHE_HOME", "npm_config_cache", "PNPM_HOME", "YARN_CACHE_FOLDER",
	"PIP_CACHE_DIR", "POETRY_CACHE_DIR", "CARGO_HOME",
}

// directEnvKeyDropped reports whether an inherited key must be removed before
// the authoritative pins are appended: secret-bearing names (T-07-06, D-12),
// Go override keys the offline pin list owns, and every toolchain root key.
func directEnvKeyDropped(key string) bool {
	for _, p := range directSecretEnvPrefixes {
		if key == p || strings.HasPrefix(key, p) {
			return true
		}
	}
	for _, k := range directGoEnvOverrides {
		if key == k {
			return true
		}
	}
	for _, k := range directRootEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// directRootEnvPins returns the thirteen job-private toolchain root pins in
// their canonical order. It is the SINGLE source for those pins: the process
// env built by buildDirectStageEnv and the bwrap-ro wrapper's --setenv block
// both derive from this function, so the two cannot drift (T-09-06). A drift
// would let a build write to a pinned path that the sandbox never bound
// read-write, or vice versa.
func directRootEnvPins(paths DirectJobPaths) [][2]string {
	return [][2]string{
		{"HOME", paths.HomeDir},
		{"TMPDIR", paths.TmpDir},
		{"GOTMPDIR", paths.TmpDir},
		{"GOCACHE", paths.GoBuildCache},
		{"GOPATH", paths.GoPath},
		{"GOMODCACHE", paths.GoModCache},
		{"XDG_CACHE_HOME", paths.CacheDir},
		{"npm_config_cache", paths.NpmCacheDir},
		{"PNPM_HOME", paths.PnpmHome},
		{"YARN_CACHE_FOLDER", paths.YarnCacheDir},
		{"PIP_CACHE_DIR", paths.PipCacheDir},
		{"POETRY_CACHE_DIR", paths.PoetryCacheDir},
		{"CARGO_HOME", paths.CargoHome},
	}
}

// buildDirectStageEnv builds the stage exec env from the current process env
// MINUS secrets, Go override keys and every toolchain root key, PLUS the
// pinned offline list and — when paths is non-nil — the job-private root pins.
// The root pins are appended after the offline list so pinned offline values
// still win (ROOT-02). A nil paths emits no root pins (used by callers that
// only need the pre-existing scrub behaviour).
func buildDirectStageEnv(offline []string, paths *DirectJobPaths) []string {
	out := make([]string, 0, len(offline)+48)
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if directEnvKeyDropped(key) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, offline...)
	if paths == nil {
		return out
	}
	for _, pin := range directRootEnvPins(*paths) {
		out = append(out, pin[0]+"="+pin[1])
	}
	return out
}

// VerifyDir runs the build and test stages for the detected project type in
// an ephemeral workspace copied from dir. It returns (report, nil) for
// verdict outcomes and (nil, err) only for caller errors such as a cancelled
// context. Directories without a recognized build configuration pass with an
// info notice.
func (d *DirectRunner) VerifyDir(ctx context.Context, dir, commitSHA string) (*VerificationReport, error) {
	return d.VerifyDirGated(ctx, dir, commitSHA, false)
}

// VerifyDirGated runs verification with fetchOK controlling whether stage-0
// dependency installation may access the network.
func (d *DirectRunner) VerifyDirGated(ctx context.Context, dir, commitSHA string, fetchOK bool) (*VerificationReport, error) {
	report := &VerificationReport{
		WorkspaceDir: dir,
		CommitSHA:    commitSHA,
		Results:      make([]ExecutionResult, 0, 2),
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cleanDir, err := filepath.Abs(dir)
	if err != nil || cleanDir == "" {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("invalid direct workspace dir %q", dir)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}
	if fi, err := os.Stat(cleanDir); err != nil || !fi.IsDir() {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("direct workspace dir unavailable: %q", dir)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}

	proj := detectDirectProject(cleanDir)
	if proj == nil {
		report.DetectedType = "generic"
		report.Status = StatusPassed
		report.Reason = "no recognized build/test configuration found; nothing to verify"
		report.Summary = "SUCCESS: No recognized build/test configuration found; nothing to verify."
		return report, nil
	}
	report.DetectedType = proj.lang
	if proj.lang == "node" {
		report.Status = StatusIncomplete
		report.Reason = "Node dependency preparation is unsupported: installers and lifecycle scripts are disabled"
		report.Summary = "INCOMPLETE: " + report.Reason
		return report, nil
	}

	// Job-private workspace/tmp/home/cache (ROOT-01): an unconfigured or
	// unsafe root fails closed before any copy or exec, so no path is ever
	// created under the shared host temporary directory (T-08-05). Provision
	// before the confinement decision so a refusal can name the job tree.
	paths, err := d.resolveJobPaths(directJobID(commitSHA))
	if err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("direct job roots unavailable: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}
	defer paths.Cleanup()

	// Single confinement decision (CONFINE-01): exactly one rank governs the
	// whole run. When every rank is unavailable the run is refused with zero
	// commands executed; there is no non-Linux and no unconfigured passthrough.
	spec, err := ResolveConfinementRank(ctx)
	if err != nil {
		report.ConfinementRank = ""
		degraded := ConfinementUnavailableReport(err.Error())
		degraded.WorkspaceDir = report.WorkspaceDir
		degraded.CommitSHA = report.CommitSHA
		degraded.DetectedType = proj.lang
		return degraded, nil
	}
	report.ConfinementRank = string(spec.Rank)
	report.ConfinementNetworkIsolated = spec.NetworkIsolated

	// Resolve the test command: first candidate whose binary exists.
	var testArgs []string
	for _, candidate := range proj.testOptions {
		if len(candidate) == 0 {
			continue
		}
		if _, err := exec.LookPath(candidate[0]); err == nil {
			testArgs = candidate
			break
		}
	}
	if len(testArgs) == 0 {
		want := ""
		if len(proj.testOptions) > 0 && len(proj.testOptions[0]) > 0 {
			want = proj.testOptions[0][0]
		}
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("%s toolchain not installed on host (direct mode requires it)", want)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}

	workDir := paths.WorkspaceDir
	if err := copyDirectory(cleanDir, workDir); err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("failed to populate ephemeral workspace: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}

	// In-root replacement check for Go (SAFE-01 encoding, D-05).
	if proj.lang == "go" {
		if err := ValidateGoMod(workDir); err != nil {
			report.Status = StatusIncomplete
			report.Reason = err.Error()
			report.Summary = fmt.Sprintf("INCOMPLETE: %v", err)
			return report, nil
		}
	}

	env := buildDirectStageEnv(proj.offline, &paths)
	if proj.lang == "node" {
		report.Status = StatusIncomplete
		report.Reason = "Node dependency preparation is unsupported: installers and lifecycle scripts are disabled"
		report.Summary = "INCOMPLETE: " + report.Reason
		return report, nil
	}

	// Lock Python scope to Go-first with honest-incomplete (D-03, D-05):
	// any Python project whose install would need network returns StatusIncomplete.
	if proj.lang == "python" && len(proj.installArgs) > 0 {
		report.FailedStage = "install"
		report.Status = StatusIncomplete
		report.Reason = "INCOMPLETE: Python network fetch not supported in stage 1 (Go-first); rerun with vendored wheels or offline --no-index"
		report.Summary = report.Reason
		return report, nil
	}

	// Stage 0: Dependency installation (warms the cache the hermetic
	// build/test stages rely on). Failure is Incomplete, not a code verdict.
	if len(proj.installArgs) > 0 {
		if _, err := exec.LookPath(proj.installArgs[0]); err != nil {
			report.Status = StatusIncomplete
			report.Reason = fmt.Sprintf("package manager %q not installed on host", proj.installArgs[0])
			report.Summary = fmt.Sprintf("INCOMPLETE: %s", report.Reason)
			return report, nil
		}
		var installRes *StageResult
		if proj.lang != "go" || !fetchOK {
			report.Status = StatusIncomplete
			report.Reason = "dependency preparation requires supported authorized gateway fetch"
			report.Summary = "INCOMPLETE: " + report.Reason
			return report, nil
		}
		installRes, err = directPrepare(ctx, d.stageTimeout(), paths, spec)
		if err != nil {
			report.Status = StatusIncomplete
			report.Reason = err.Error()
			report.Summary = "INCOMPLETE: " + report.Reason
			return report, nil
		}
		report.Results = append(report.Results, directExecutionResult(installRes))
		if installRes.Truncated {
			report.Truncated = true
			report.DroppedBytes += installRes.DroppedBytes
		}
		if !installRes.Passed {
			report.FailedStage = "install"
			report.Status = StatusIncomplete
			report.Reason = fmt.Sprintf("dependency installation failed: %s", strings.Join(proj.installArgs, " "))
			report.Summary = fmt.Sprintf("INCOMPLETE: %s", report.Reason)
			return report, nil
		}
	}

	if len(proj.buildArgs) > 0 {
		buildArgv, err := spec.WrapArgv(proj.buildArgs, paths, WrapOptions{})
		if err != nil {
			return confinementWrapFailureReport(report, err), nil
		}
		buildRes := runDirectStage(ctx, d.stageTimeout(), workDir, "build", buildArgv, spec.Env(env, paths))
		report.Results = append(report.Results, directExecutionResult(buildRes))
		if buildRes.Truncated {
			report.Truncated = true
			report.DroppedBytes += buildRes.DroppedBytes
		}
		if !buildRes.Passed {
			report.FailedStage = "build"
			mapDirectStageFailure(report, "build", buildRes,
				"build stage exceeded execution timeout",
				"TIMEOUT: build stage timed out.",
				"filesystem disk quota exceeded during build",
				"DISK_EXHAUSTED: build failed with ENOSPC.",
				"memory limit exceeded during build",
				"RESOURCE_EXHAUSTED: build exceeded memory limits.",
				fmt.Sprintf("%s failed to compile", strings.Join(proj.buildArgs, " ")),
				fmt.Sprintf("FAILED: %s failed.", strings.Join(proj.buildArgs, " ")))
			return report, nil
		}
	}

	stageTestArgv, err := spec.WrapArgv(testArgs, paths, WrapOptions{})
	if err != nil {
		return confinementWrapFailureReport(report, err), nil
	}
	testRes := runDirectStage(ctx, d.stageTimeout(), workDir, "test", stageTestArgv, spec.Env(env, paths))
	report.Results = append(report.Results, directExecutionResult(testRes))
	if testRes.Truncated {
		report.Truncated = true
		report.DroppedBytes += testRes.DroppedBytes
	}
	if !testRes.Passed {
		report.FailedStage = "test"
		mapDirectStageFailure(report, "test", testRes,
			"test stage exceeded execution timeout",
			"TIMEOUT: test stage timed out.",
			"filesystem disk quota exceeded during test",
			"DISK_EXHAUSTED: test failed with ENOSPC.",
			"memory limit exceeded during test",
			"RESOURCE_EXHAUSTED: test exceeded memory limits.",
			fmt.Sprintf("%s reported failures", strings.Join(testArgs, " ")),
			fmt.Sprintf("FAILED: %s reported failures.", strings.Join(testArgs, " ")))
		return report, nil
	}

	report.Status = StatusPassed
	report.Summary = "SUCCESS: Project builds and all tests pass cleanly."
	return report, nil
}

// runDirectStage executes one whitelisted stage in dir with the given timeout.
// It is unexported and only ever called with the exact argv from
// detectDirectProject — no shell, no caller-supplied commands.
func runDirectStage(ctx context.Context, timeout time.Duration, dir, stageName string, args, env []string) *StageResult {
	if len(args) == 0 {
		return &StageResult{StageName: stageName, Command: stageName, ExitCode: 1}
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(stageCtx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	// Detach from stdin so a hostile test cannot block on input.
	cmd.Stdin = nil

	stdoutCollector := NewLimitedCollector(DefaultMaxRetainedBytes)
	stderrCollector := NewLimitedCollector(DefaultMaxRetainedBytes)
	cmd.Stdout = stdoutCollector
	cmd.Stderr = stderrCollector

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	passed := true
	isTimeout := false
	isOOM := false
	isDiskFull := false

	if err != nil {
		passed = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
		if stageCtx.Err() == context.DeadlineExceeded {
			isTimeout = true
			exitCode = 124
		}
		lowerStderr := strings.ToLower(stderrCollector.String())
		if strings.Contains(lowerStderr, "no space left on device") || strings.Contains(lowerStderr, "enospc") {
			isDiskFull = true
		}
		if exitCode == 137 || strings.Contains(lowerStderr, "out of memory") || strings.Contains(lowerStderr, "killed") {
			isOOM = true
		}
	}

	return &StageResult{
		StageName:    stageName,
		Command:      strings.Join(args, " "),
		ExitCode:     exitCode,
		Stdout:       stdoutCollector.Formatted("stdout"),
		Stderr:       stderrCollector.Formatted("stderr"),
		Duration:     duration,
		Passed:       passed,
		Truncated:    stdoutCollector.Truncated() || stderrCollector.Truncated(),
		DroppedBytes: stdoutCollector.DroppedBytes() + stderrCollector.DroppedBytes(),
		IsTimeout:    isTimeout,
		IsOOM:        isOOM,
		IsDiskFull:   isDiskFull,
	}
}

// stageTimeout returns the configured per-stage timeout.
func (d *DirectRunner) stageTimeout() time.Duration {
	if d == nil || d.Timeout <= 0 {
		return 5 * time.Minute
	}
	return d.Timeout
}

func directExecutionResult(res *StageResult) ExecutionResult {
	return ExecutionResult{
		Command:      res.Command,
		Stdout:       res.Stdout,
		Stderr:       res.Stderr,
		ExitCode:     res.ExitCode,
		Duration:     res.Duration,
		Passed:       res.Passed,
		Truncated:    res.Truncated,
		DroppedBytes: res.DroppedBytes,
	}
}

func mapDirectStageFailure(report *VerificationReport, stage string, res *StageResult,
	timeoutReason, timeoutSummary, diskReason, diskSummary, oomReason, oomSummary,
	failReason, failSummary string) {
	switch {
	case res.IsTimeout:
		report.Status = StatusTimeout
		report.Reason = timeoutReason
		report.Summary = timeoutSummary
	case res.IsDiskFull:
		report.Status = StatusDiskExhausted
		report.Reason = diskReason
		report.Summary = diskSummary
	case res.IsOOM:
		report.Status = StatusResourceExhausted
		report.Reason = oomReason
		report.Summary = oomSummary
	default:
		if stage == "build" {
			report.Status = StatusBuildFailed
		} else {
			report.Status = StatusTestFailed
		}
		report.Reason = failReason
		report.Summary = failSummary
	}
}

// runDirectSnapshot executes a validated snapshot through the direct path.
// Podman, slot leasing, and attestation are skipped; the snapshot source is
// copied into an ephemeral workspace and verified there.
func (r *Runner) runDirectSnapshot(ctx context.Context, snapshot *Snapshot, fetchOK bool) (*VerificationReport, error) {
	direct := r.Direct
	if direct == nil {
		direct = NewDirectRunner(r.executionTimeout())
	}
	report, err := direct.VerifyDirGated(ctx, snapshot.SourceDir, snapshot.CommitSHA, fetchOK)
	if err != nil {
		return report, err
	}
	r.extractCustomRules(snapshot.SourceDir, report)
	return report, nil
}

// In-loop command execution (Phase 06-03, D-03).
//
// InLoopCommand is the constrained exec entry for the gated ReAct loop's
// run_command tool in direct mode. Commands run in the SAME work copy that
// holds the edits (the caller passes the work-copy dir; VerifyDir-style
// ephemeral copying is the outer pipeline's job, not the inner loop's).
// Argv is resolved from the shared detectDirectProject matrix: only the
// build/test rows for the detected project type may run. No shell, no
// caller-supplied commands, cwd pinned to workDir, stdin detached, per-call
// timeout, output capped. A passing inner command is advisory only; only the
// outer platformRunner.RunSnapshot green authorizes push.
const defaultInLoopMaxOutputBytes = 6000

// ErrInLoopToolchainMissing reports a matrix-allowed binary absent from PATH
// (direct) or from the container (podman exec exit 127). Callers map it to an
// honest-unavailable completion (StatusUnavailable, never retried), never to
// a failed verdict.
var ErrInLoopToolchainMissing = errors.New("toolchain not installed")

// inLoopMatrixMismatch is the shared rejection text both backends return
// when argv is not a matrix row for the project.
const inLoopMatrixMismatch = "in-loop command not in project matrix"

// inLoopConfig holds the caller-supplied options for one InLoopCommand call.
type inLoopConfig struct {
	roots DirectRoots
}

// InLoopOption customises one InLoopCommand call.
type InLoopOption func(*inLoopConfig)

// WithDirectRoots supplies the operator-configured job-private roots the
// in-loop command must provision. Without it InLoopCommand fails closed with
// ErrConfinementUnavailable rather than falling back to an ambient root.
func WithDirectRoots(roots DirectRoots) InLoopOption {
	return func(c *inLoopConfig) { c.roots = roots }
}

// InLoopCommand runs one matrix-constrained build/test command in workDir.
// It returns (stdout, stderr, exitCode, nil) for executed commands — a
// nonzero exit is an observation, not an error — and ("", "", -1, err) for
// gate rejections or (..., 127, err) wrapping ErrInLoopToolchainMissing when
// the matrix-allowed binary is absent.
func InLoopCommand(ctx context.Context, workDir string, argv []string, timeout time.Duration, maxOutputBytes int, opts ...InLoopOption) (string, string, int, error) {
	cfg := inLoopConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := validateInLoopArgv(argv); err != nil {
		return "", "", -1, err
	}
	if err := ctx.Err(); err != nil {
		return "", "", -1, err
	}
	proj := detectDirectProject(workDir)
	if proj == nil {
		return "", "", -1, fmt.Errorf("%s: no recognized project in %q", inLoopMatrixMismatch, workDir)
	}
	if !inLoopArgvAllowed(argv, inLoopAllowedCommands(proj)) {
		return "", "", -1, fmt.Errorf("%s: %q", inLoopMatrixMismatch, strings.Join(argv, " "))
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		return "", "", 127, fmt.Errorf("%w: %q is missing on PATH (in-loop command unavailable)", ErrInLoopToolchainMissing, argv[0])
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultInLoopMaxOutputBytes
	}
	// Single confinement decision (CONFINE-01): the in-loop exec entry routes
	// through the same ResolveConfinementRank as VerifyDirGated. A host with
	// no usable rank is refused here rather than run unconfined.
	spec, err := ResolveConfinementRank(ctx)
	if err != nil {
		return "", "", -1, err
	}
	if strings.TrimSpace(cfg.roots.WorkRoot) == "" || strings.TrimSpace(cfg.roots.CacheRoot) == "" {
		return "", "", -1, fmt.Errorf("%w: no operator job roots supplied (WithDirectRoots is required)", ErrConfinementUnavailable)
	}
	// Job-private tmp/home/cache instead of the shared host TMPDIR (ROOT-01).
	paths, err := provisionDirectJobDirs(cfg.roots, directJobID("inloop"))
	if err != nil {
		return "", "", -1, fmt.Errorf("direct job roots unavailable: %w", err)
	}
	defer paths.Cleanup()
	// The in-loop command runs in the caller's edit work copy, not the
	// freshly provisioned empty job workspace: bind that tree as /work so the
	// command sees exactly the files the edits landed in.
	paths.WorkspaceDir = workDir
	execArgv, err := spec.WrapArgv(argv, paths, WrapOptions{})
	if err != nil {
		return "", "", -1, err
	}
	env := spec.Env(buildDirectStageEnv(proj.offline, &paths), paths)
	return runInLoopDirectStage(ctx, timeout, workDir, execArgv, env, maxOutputBytes)
}

// validateInLoopArgv applies the element-level gate shared by both backends:
// non-empty argv, no shell metacharacters, no parent-directory traversal.
// Matrix-row membership is checked separately by each backend.
func validateInLoopArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("in-loop command argv is empty")
	}
	for i, el := range argv {
		if el == "" {
			return fmt.Errorf("in-loop command argv element %d is empty", i)
		}
		if strings.ContainsAny(el, ";&|$`\\\n\r") {
			return fmt.Errorf("in-loop command rejected: shell metacharacter in argv element %d", i)
		}
		if el == ".." || strings.HasPrefix(el, "../") || strings.HasPrefix(el, `..\`) ||
			strings.Contains(el, "/../") || strings.Contains(el, `\..\`) {
			return fmt.Errorf("in-loop command rejected: parent-directory traversal in argv element %d", i)
		}
	}
	return nil
}

// inLoopAllowedCommands returns the runnable rows for one detected project:
// the build argv plus every test-option row. Install rows are deliberately
// excluded (network fetchers never run in-loop). Go additionally allows
// vet, per the Phase 6 matrix (go build/vet/test).
func inLoopAllowedCommands(proj *directProject) [][]string {
	if proj == nil {
		return nil
	}
	var out [][]string
	if len(proj.buildArgs) > 0 {
		out = append(out, append([]string(nil), proj.buildArgs...))
	}
	for _, candidate := range proj.testOptions {
		if len(candidate) > 0 {
			out = append(out, append([]string(nil), candidate...))
		}
	}
	if proj.lang == "go" {
		out = append(out, []string{"go", "vet", "./..."})
	}
	return out
}

// inLoopArgvAllowed reports whether argv exactly equals one allowed row.
func inLoopArgvAllowed(argv []string, allowed [][]string) bool {
	for _, row := range allowed {
		if len(row) != len(argv) {
			continue
		}
		match := true
		for i := range row {
			if row[i] != argv[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// runInLoopDirectStage mirrors runDirectStage (fixed argv only,
// exec.CommandContext, cmd.Dir pinned, stdin detached) with a
// caller-supplied per-stream byte cap and the in-loop truncation marker.
// runDirectStage stays unexported and untouched: its 100 KiB Formatted
// marker would be the wrong text here, so the equivalent is replicated with
// the exact plan marker instead of re-truncating its formatted strings.
func runInLoopDirectStage(ctx context.Context, timeout time.Duration, dir string, argv, env []string, maxOutputBytes int) (string, string, int, error) {
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(stageCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	// Detach from stdin so a hostile test cannot block on input.
	cmd.Stdin = nil

	stdoutCollector := NewLimitedCollector(maxOutputBytes)
	stderrCollector := NewLimitedCollector(maxOutputBytes)
	cmd.Stdout = stdoutCollector
	cmd.Stderr = stderrCollector

	runErr := cmd.Run()
	stdout := formatInLoopOutput(stdoutCollector, maxOutputBytes)
	stderr := formatInLoopOutput(stderrCollector, maxOutputBytes)

	if runErr != nil {
		exitCode := 1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if stageCtx.Err() == context.DeadlineExceeded {
			return stdout, stderr, 124, stageCtx.Err()
		}
		return stdout, stderr, exitCode, nil
	}
	return stdout, stderr, 0, nil
}

// formatInLoopOutput renders one capped stream with the exact in-loop
// truncation marker shared by both backends.
func formatInLoopOutput(c *LimitedCollector, maxOutputBytes int) string {
	s := c.String()
	if c.Truncated() {
		s += fmt.Sprintf("\n...[truncated: command output exceeds %d bytes]...", maxOutputBytes)
	}
	return s
}
