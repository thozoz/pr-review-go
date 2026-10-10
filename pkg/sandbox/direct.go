package sandbox

import (
	"context"
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
//   - Each job runs in an ephemeral os.MkdirTemp workspace copied from the
//     snapshot; the snapshot source is never mutated.
//   - Per-stage timeout via context; output capped by LimitedCollector.
//   - Same verification gate: non-passed status means changes are never
//     committed or pushed by callers.
type DirectRunner struct {
	Timeout time.Duration
}

// NewDirectRunner creates a DirectRunner with the given per-stage timeout.
// Non-positive timeouts fall back to the 5-minute execution default.
func NewDirectRunner(timeout time.Duration) *DirectRunner {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &DirectRunner{Timeout: timeout}
}

// directProject describes the verified commands for one project type.
type directProject struct {
	lang      string
	tool      string
	buildArgs []string // nil when the type has no separate build step
	testArgs  []string
	offline   []string // extra hermetic env overrides
}

// directProjectTable maps marker files to their verification commands.
func detectDirectProject(dir string) *directProject {
	if fileExists(filepath.Join(dir, "go.mod")) {
		return &directProject{
			lang:      "go",
			tool:      "go",
			buildArgs: []string{"go", "build", "./..."},
			testArgs:  []string{"go", "test", "-race", "./..."},
			offline:   []string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOVCS=off"},
		}
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		return &directProject{lang: "node", tool: "npm", testArgs: []string{"npm", "test"}}
	}
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return &directProject{
			lang:     "rust",
			tool:     "cargo",
			testArgs: []string{"cargo", "test"},
			offline:  []string{"CARGO_NET_OFFLINE=true"},
		}
	}
	if fileExists(filepath.Join(dir, "pyproject.toml")) || fileExists(filepath.Join(dir, "requirements.txt")) {
		return &directProject{lang: "python", tool: "pytest", testArgs: []string{"pytest"}}
	}
	return nil
}

// VerifyDir runs the build and test stages for the detected project type in
// an ephemeral workspace copied from dir. It returns (report, nil) for
// verdict outcomes and (nil, err) only for caller errors such as a cancelled
// context. Directories without a recognized build configuration pass with an
// info notice.
func (d *DirectRunner) VerifyDir(ctx context.Context, dir, commitSHA string) (*VerificationReport, error) {
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

	if _, err := exec.LookPath(proj.tool); err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("%s toolchain not installed on host (direct mode requires it)", proj.tool)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}

	// Ephemeral per-job workspace: the snapshot source is copied and never
	// mutated by build/test execution.
	workDir, err := os.MkdirTemp("", "pr-review-workspace-*")
	if err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("failed to create ephemeral workspace: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %s", report.Reason)
		return report, nil
	}
	defer func() { _ = os.RemoveAll(workDir) }()
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

	env := append(os.Environ(), proj.offline...)

	if len(proj.buildArgs) > 0 {
		buildRes := runDirectStage(ctx, d.stageTimeout(), workDir, "build", proj.buildArgs, env)
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

	testRes := runDirectStage(ctx, d.stageTimeout(), workDir, "test", proj.testArgs, env)
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
			fmt.Sprintf("%s reported failures", strings.Join(proj.testArgs, " ")),
			fmt.Sprintf("FAILED: %s reported failures.", strings.Join(proj.testArgs, " ")))
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
func (r *Runner) runDirectSnapshot(ctx context.Context, snapshot *Snapshot) (*VerificationReport, error) {
	direct := r.Direct
	if direct == nil {
		direct = NewDirectRunner(r.executionTimeout())
	}
	report, err := direct.VerifyDir(ctx, snapshot.SourceDir, snapshot.CommitSHA)
	if err != nil {
		return report, err
	}
	r.extractCustomRules(snapshot.SourceDir, report)
	return report, nil
}
