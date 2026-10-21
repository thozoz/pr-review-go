package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
)

var (
	ErrEscapingReplacement   = errors.New("replacement directive escapes module root")
	ErrIncompleteDependency  = errors.New("incomplete dependency requirement")
)

// ValidateGoMod parses the go.mod file in workDir and validates that all local
// replacement paths remain confined within the module root directory (SAFE-01 encoding, D-05).
func ValidateGoMod(workDir string) error {
	cleanWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}

	goModPath := filepath.Join(cleanWorkDir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	inReplaceBlock := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		if line == "replace (" {
			inReplaceBlock = true
			continue
		}
		if inReplaceBlock && line == ")" {
			inReplaceBlock = false
			continue
		}

		var replaceClause string
		if inReplaceBlock {
			replaceClause = line
		} else {
			fields := strings.Fields(line)
			if len(fields) > 1 && fields[0] == "replace" {
				// Trim the leading "replace" token and any following whitespace
				trimmed := strings.TrimSpace(line)
				trimmed = strings.TrimPrefix(trimmed, "replace")
				replaceClause = strings.TrimSpace(trimmed)
			}
		}

		if replaceClause != "" {
			if err := checkReplaceClause(cleanWorkDir, replaceClause); err != nil {
				return err
			}
		}
	}

	return scanner.Err()
}

// isPythonProject reports whether a directory contains Python project descriptors.
func isPythonProject(dir string) bool {
	for _, f := range []string{"pyproject.toml", "requirements.txt", "setup.py", "poetry.lock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// isRemoteModulePath reports whether a path looks like a remote module path (e.g., has a dotted first path element like example.com/pkg).
func isRemoteModulePath(p string) bool {
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	parts := strings.Split(p, "/")
	if len(parts) == 0 {
		return false
	}
	return strings.Contains(parts[0], ".")
}

func checkReplaceClause(workDir, clause string) error {
	parts := strings.Split(clause, "=>")
	if len(parts) != 2 {
		return nil
	}

	// Source module path is the first field of the left-hand side
	// (e.g. "example.com/old" in "replace example.com/old v1.0.0 => ...").
	source := strings.TrimSpace(parts[0])
	if idx := strings.Index(source, "//"); idx != -1 {
		source = strings.TrimSpace(source[:idx])
	}
	sourceMod := ""
	if sourceFields := strings.Fields(source); len(sourceFields) > 0 {
		sourceMod = sourceFields[0]
	}

	target := strings.TrimSpace(parts[1])
	// Split off any comments
	if idx := strings.Index(target, "//"); idx != -1 {
		target = strings.TrimSpace(target[:idx])
	}

	fields := strings.Fields(target)
	if len(fields) == 0 {
		return nil
	}

	// In Go modules, a two-field target (e.g. "example.com/mod v1.2.3") is a
	// module@version replacement. Pinning the SAME module to a version stays
	// inside the go.sum-verified fetch set; swapping to a DIFFERENT remote
	// module dodges go.sum, so it is refused in-root-escape style (D-05).
	if len(fields) > 1 {
		if sourceMod != "" && fields[0] != sourceMod {
			return fmt.Errorf("%w: remote module swap %q => %q dodges checksum verification", ErrEscapingReplacement, sourceMod, fields[0])
		}
		return nil
	}

	targetPath := fields[0]
	// A single-field target reading as a remote module path (dotted first
	// segment, e.g. "example.com/other") is the versionless form of the same
	// swap — refuse unless it names the source module itself. Genuine local
	// paths (./sub, sub/pkg, /abs) never match this shape.
	if targetPath != sourceMod && isRemoteModulePath(targetPath) {
		return fmt.Errorf("%w: remote module replacement target %q", ErrEscapingReplacement, targetPath)
	}
	// If it starts with "." or "/" or "\" or contains path separators without being a module domain
	if strings.HasPrefix(targetPath, ".") || strings.HasPrefix(targetPath, "/") || strings.HasPrefix(targetPath, "\\") || strings.Contains(targetPath, "/") || strings.Contains(targetPath, "\\") {
		// Absolute path is rejected
		if filepath.IsAbs(targetPath) {
			return fmt.Errorf("%w: absolute replacement target %q", ErrEscapingReplacement, targetPath)
		}

		resolved := filepath.Clean(filepath.Join(workDir, targetPath))
		rel, err := filepath.Rel(workDir, resolved)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%w: replacement target %q escapes module root", ErrEscapingReplacement, targetPath)
		}
	}

	return nil
}

// DependencyPreparer defines the contract for downloading Go dependencies through
// the narrow gateway before entering offline build and test stages.
type DependencyPreparer interface {
	PrepareDependencies(ctx context.Context, jobID string, lease *SlotLease) (*StageResult, error)
}

// PodmanDependencyPreparer executes dependency download in an isolated, network-none container
// with trusted loopback relay to the per-job Unix dependency gateway (D-05, D-06, D-07, D-12).
type PodmanDependencyPreparer struct {
	Backend    *PodmanBackend
	Config     *config.Config
	GatewayCfg DependencyGatewayConfig
	FetchArgv  []string
}

// ValidateFetchArgv gates stage-1 fetch commands (D-05, SAFE-01):
// only the exact triple ["go", "mod", "download"] passes.
// Any extra flag, tool (curl, wget, go get), or argv element attempting
// environment overrides (e.g. GOFLAGS=..., GOPROXY=...) is rejected.
func ValidateFetchArgv(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("fetch argv is empty")
	}
	for _, arg := range argv {
		if strings.HasPrefix(arg, "GOFLAGS=") || strings.HasPrefix(arg, "GOPROXY=") {
			return fmt.Errorf("fetch argv rejected: environment override in argv: %s", arg)
		}
	}
	if len(argv) != 3 {
		return fmt.Errorf("fetch argv rejected: expected exact 3 elements [go mod download], got %d elements %v", len(argv), argv)
	}
	if argv[0] != "go" || argv[1] != "mod" || argv[2] != "download" {
		return fmt.Errorf("fetch argv rejected: must be exactly [go mod download], got %v", argv)
	}
	return nil
}

func NewDependencyPreparer(backend *PodmanBackend, cfg *config.Config) *PodmanDependencyPreparer {
	return &PodmanDependencyPreparer{
		Backend: backend,
		Config:  cfg,
	}
}

func (p *PodmanDependencyPreparer) PrepareDependencies(ctx context.Context, jobID string, lease *SlotLease) (*StageResult, error) {
	if lease == nil {
		return nil, errors.New("slot lease is nil")
	}

	// 1. Validate go.mod replacements remain in-root
	if err := ValidateGoMod(lease.WorkDir); err != nil {
		return &StageResult{
			StageName:    "prep",
			Passed:       false,
			IsIncomplete: true,
			Stderr:       fmt.Sprintf("INCOMPLETE: %v", err),
		}, nil
	}

	// 2. Check if go.mod exists
	goModPath := filepath.Join(lease.WorkDir, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		// Python projects return honest-incomplete: Python network fetch is not supported in stage 1 (D-03, D-05)
		if isPythonProject(lease.WorkDir) {
			return &StageResult{
				StageName:    "prep",
				Passed:       false,
				IsIncomplete: true,
				Stderr:       "INCOMPLETE: Python network fetch not supported in stage 1 (Go-first); rerun with vendored wheels or offline --no-index",
			}, nil
		}
		// No go.mod; nothing to prepare
		return &StageResult{
			StageName: "prep",
			Passed:    true,
		}, nil
	}

	// If project is vendored, no download needed
	if _, err := os.Stat(filepath.Join(lease.WorkDir, "vendor", "modules.txt")); err == nil {
		return &StageResult{
			StageName: "prep",
			Passed:    true,
		}, nil
	}

	if p.Backend == nil {
		return nil, errors.New("container backend is unconfigured")
	}

	// Validate stage-1 fetch command before launching container (D-05, SAFE-01)
	fetchArgv := p.FetchArgv
	if len(fetchArgv) == 0 {
		fetchArgv = []string{"go", "mod", "download"}
	}
	if err := ValidateFetchArgv(fetchArgv); err != nil {
		return &StageResult{
			StageName:    "prep",
			Passed:       false,
			IsIncomplete: true,
			Stderr:       fmt.Sprintf("INCOMPLETE: %v", err),
		}, nil
	}

	// 3. Start per-job dependency gateway
	gwSockPath := filepath.Join(lease.ControlDir, fmt.Sprintf("depgw-%s.sock", jobID))
	gwCfg := p.GatewayCfg
	gwCfg.SocketPath = gwSockPath
	if gwCfg.RequestTimeout <= 0 {
		if p.Config != nil && p.Config.SandboxTimeoutPrep > 0 {
			gwCfg.RequestTimeout = p.Config.SandboxTimeoutPrep
		} else {
			gwCfg.RequestTimeout = 3 * time.Minute
		}
	}

	gw, err := StartDependencyGateway(gwCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to start dependency gateway: %w", err)
	}
	defer gw.Close()

	// 4. Construct Podman container specification
	containerName := fmt.Sprintf("pr-rev-%s-prep", jobID)
	podmanArgs := []string{
		"run",
		"--name", containerName,
		"--label", fmt.Sprintf("pr-review.job_id=%s", jobID),
		"--label", "pr-review.stage=prep",
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pid=private",
		"--ipc=none",
		"--cgroupns=private",
	}
	if p.Backend == nil || !p.Backend.cfg.AllowRootfulSandbox {
		podmanArgs = append(podmanArgs, "--userns=keep-id")
	}
	podmanArgs = append(podmanArgs,
		"--read-only",
		"--read-only-tmpfs=false",
		"--log-driver=none",
		"--pull=never",
		fmt.Sprintf("--cpus=%f", p.Backend.cfg.CPUs),
		fmt.Sprintf("--memory=%d", p.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--memory-swap=%d", p.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--pids-limit=%d", p.Backend.cfg.PidsLimit),
		"-v", fmt.Sprintf("%s:/run/sandbox/gateway.sock:ro", gwSockPath),
		"-v", fmt.Sprintf("%s:/work:rw", lease.WorkDir),
		"-v", fmt.Sprintf("%s:/cache:rw", lease.CacheDir),
		"-v", fmt.Sprintf("%s:/tmp:rw", lease.TmpDir),
		"-w", "/work",
		"--entrypoint=",
		"-e", "HOME=/tmp",
		"-e", "TMPDIR=/tmp",
		p.Backend.cfg.ImageDigest,
		"pr-review-sandbox-helper", "prepare",
		"-socket", "/run/sandbox/gateway.sock",
		"-work", "/work",
	)

	prepTimeout := 3 * time.Minute
	if p.Config != nil && p.Config.SandboxTimeoutPrep > 0 {
		prepTimeout = p.Config.SandboxTimeoutPrep
	}
	prepCtx, cancelPrep := context.WithTimeout(ctx, prepTimeout)
	defer cancelPrep()

	cmd := exec.CommandContext(prepCtx, p.Backend.cfg.BinaryPath, podmanArgs...)

	stdoutCollector := NewLimitedCollector(DefaultMaxRetainedBytes)
	stderrCollector := NewLimitedCollector(DefaultMaxRetainedBytes)
	cmd.Stdout = stdoutCollector
	cmd.Stderr = stderrCollector

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	// Clean up container immediately
	cleanCtx, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	_ = p.Backend.StopAndKillJobContainers(cleanCtx, jobID)

	exitCode := 0
	passed := true
	isTimeout := false
	isOOM := false
	isDiskFull := false
	isIncomplete := false

	stderrStr := stderrCollector.String()
	lowerStderr := strings.ToLower(stderrStr)

	if runErr != nil {
		passed = false
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}

		if prepCtx.Err() == context.DeadlineExceeded {
			isTimeout = true
			exitCode = 124
		}

		if strings.Contains(lowerStderr, "no space left on device") || strings.Contains(lowerStderr, "enospc") || exitCode == 3 {
			isDiskFull = true
		}
		if exitCode == 137 || strings.Contains(lowerStderr, "out of memory") || strings.Contains(lowerStderr, "killed") {
			isOOM = true
		}
		if exitCode == 2 || strings.Contains(stderrStr, "INCOMPLETE:") {
			isIncomplete = true
		}
	}

	result := &StageResult{
		StageName:    "prep",
		Command:      "pr-review-sandbox-helper prepare",
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
		IsIncomplete: isIncomplete,
	}

	return result, nil
}
