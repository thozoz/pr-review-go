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
		} else if strings.HasPrefix(line, "replace ") {
			replaceClause = strings.TrimPrefix(line, "replace ")
		}

		if replaceClause != "" {
			if err := checkReplaceClause(cleanWorkDir, replaceClause); err != nil {
				return err
			}
		}
	}

	return scanner.Err()
}

func checkReplaceClause(workDir, clause string) error {
	parts := strings.Split(clause, "=>")
	if len(parts) != 2 {
		return nil
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

	// In Go modules, if the target has two fields (e.g. "example.com/mod v1.2.3"),
	// it is a module@version replacement, not a local filesystem replacement.
	// A local filesystem replacement has exactly one field (a path).
	if len(fields) > 1 {
		return nil
	}

	targetPath := fields[0]
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
		"--userns=keep-id",
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
	}

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
