package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var (
	ErrPodmanUnavailable       = errors.New("rootless podman runtime is unavailable")
	ErrPodmanAttestationFailed = errors.New("podman capability attestation failed")
)

type PodmanConfig struct {
	BinaryPath   string
	ImageDigest  string
	CPUs         float64
	MemoryBytes  int64
	PidsLimit    int64
	TimeoutStage time.Duration
	TimeoutClean time.Duration
	// AllowRootfulSandbox opts out of the mandatory rootless check in
	// Attest. Only set in pre-isolated environments (unprivileged LXC,
	// CI runners, Kubernetes pods) where nested user namespaces are
	// unavailable — never on a shared bare-metal host.
	AllowRootfulSandbox bool
}

type PodmanBackend struct {
	cfg PodmanConfig
}

type StageResult struct {
	StageName    string        `json:"stage_name"`
	Command      string        `json:"command"`
	ExitCode     int           `json:"exit_code"`
	Stdout       string        `json:"stdout"`
	Stderr       string        `json:"stderr"`
	Duration     time.Duration `json:"duration"`
	Passed       bool          `json:"passed"`
	Truncated    bool          `json:"truncated"`
	DroppedBytes int64         `json:"dropped_bytes"`
	IsTimeout    bool          `json:"is_timeout"`
	IsOOM        bool          `json:"is_oom"`
	IsDiskFull   bool          `json:"is_disk_full"`
	IsIncomplete bool          `json:"is_incomplete"`
}

func NewPodmanBackend(cfg PodmanConfig) *PodmanBackend {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "podman"
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = 2.0
	}
	if cfg.MemoryBytes <= 0 {
		cfg.MemoryBytes = 2147483648 // 2 GiB
	}
	if cfg.PidsLimit <= 0 {
		cfg.PidsLimit = 256
	}
	if cfg.TimeoutStage <= 0 {
		cfg.TimeoutStage = 5 * time.Minute
	}
	if cfg.TimeoutClean <= 0 {
		cfg.TimeoutClean = 15 * time.Second
	}
	return &PodmanBackend{
		cfg: cfg,
	}
}

// podmanInfoHost models a subset of `podman info --format json`.
type podmanInfoOutput struct {
	Host struct {
		Rootless      bool   `json:"rootless"`
		CgroupVersion string `json:"cgroupVersion"`
		Cgroups       struct {
			Controllers []string `json:"controllers"`
		} `json:"cgroups"`
		RemoteSocket struct {
			Exists bool `json:"exists"`
		} `json:"remoteSocket"`
		Security struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
	} `json:"host"`
}

// Attest verifies local rootless Podman execution, cgroup-v2 support, and resource delegation.
func (p *PodmanBackend) Attest(ctx context.Context) error {
	// 1. Check podman binary and info
	infoCmd := exec.CommandContext(ctx, p.cfg.BinaryPath, "info", "--format", "json")
	var stdout, stderr bytes.Buffer
	infoCmd.Stdout = &stdout
	infoCmd.Stderr = &stderr
	if err := infoCmd.Run(); err != nil {
		return fmt.Errorf("%w: podman info failed: %v (stderr: %s)", ErrPodmanUnavailable, err, stderr.String())
	}

	var info podmanInfoOutput
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return fmt.Errorf("%w: failed to parse podman info: %v", ErrPodmanAttestationFailed, err)
	}

	// 2. Deny rootful Podman (D-01, SAFE-01), unless the operator
	// explicitly opted out for a pre-isolated environment.
	if !info.Host.Rootless && !p.cfg.AllowRootfulSandbox {
		return fmt.Errorf("%w: rootful podman is denied; only local rootless execution is permitted (override with ALLOW_ROOTFUL_SANDBOX=1 in pre-isolated environments)", ErrPodmanAttestationFailed)
	}
	if !info.Host.Rootless {
		log.Println("[sandbox] WARNING: Running with rootful Podman sandbox (ALLOW_ROOTFUL_SANDBOX=1 enabled). Ensure host environment is already container-isolated.")
	}

	// 3. Verify cgroup-v2
	cgroupVer := strings.ToLower(strings.TrimSpace(info.Host.CgroupVersion))
	if cgroupVer != "2" && cgroupVer != "v2" {
		return fmt.Errorf("%w: cgroup-v2 is required, got %q", ErrPodmanAttestationFailed, info.Host.CgroupVersion)
	}

	// 4. Verify image digest is configured
	if p.cfg.ImageDigest == "" {
		return fmt.Errorf("%w: trusted image digest is not configured", ErrPodmanAttestationFailed)
	}

	// 5. Run live capability probe with limits applied
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	probeCmd := exec.CommandContext(probeCtx, p.cfg.BinaryPath,
		"run", "--rm",
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pid=private",
		"--ipc=none",
		"--cgroupns=private",
		"--read-only",
		"--read-only-tmpfs=false",
		"--log-driver=none",
		"--pull=never",
		fmt.Sprintf("--cpus=%f", p.cfg.CPUs),
		fmt.Sprintf("--memory=%d", p.cfg.MemoryBytes),
		fmt.Sprintf("--memory-swap=%d", p.cfg.MemoryBytes),
		fmt.Sprintf("--pids-limit=%d", p.cfg.PidsLimit),
		"--entrypoint=",
		p.cfg.ImageDigest,
		"cat", "/sys/fs/cgroup/memory.max",
	)

	var probeOut, probeErr bytes.Buffer
	probeCmd.Stdout = &probeOut
	probeCmd.Stderr = &probeErr
	if err := probeCmd.Run(); err != nil {
		return fmt.Errorf("%w: live capability probe failed: %v (stderr: %s)", ErrPodmanAttestationFailed, err, probeErr.String())
	}

	// Verify the reported memory.max matches the configured byte limit
	reportedMemStr := strings.TrimSpace(probeOut.String())
	reportedMem, err := strconv.ParseInt(reportedMemStr, 10, 64)
	if err == nil {
		if reportedMem != p.cfg.MemoryBytes {
			return fmt.Errorf("%w: cgroup memory.max (%d) does not match configured limit (%d)",
				ErrPodmanAttestationFailed, reportedMem, p.cfg.MemoryBytes)
		}
	}

	return nil
}

// RunStage executes a single verification stage inside an isolated container.
func (p *PodmanBackend) RunStage(ctx context.Context, jobID, stageName string, lease *SlotLease, args []string) (*StageResult, error) {
	if lease == nil {
		return nil, fmt.Errorf("slot lease is nil")
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("stage args are empty")
	}

	containerName := fmt.Sprintf("pr-rev-%s-%s", jobID, stageName)

	podmanArgs := []string{
		"run",
		"--name", containerName,
		"--label", fmt.Sprintf("pr-review.job_id=%s", jobID),
		"--label", fmt.Sprintf("pr-review.stage=%s", stageName),
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pid=private",
		"--ipc=none",
		"--cgroupns=private",
	}
	if !p.cfg.AllowRootfulSandbox {
		podmanArgs = append(podmanArgs, "--userns=keep-id")
	}
	podmanArgs = append(podmanArgs,
		"--read-only",
		"--read-only-tmpfs=false",
		"--log-driver=none",
		"--pull=never",
		fmt.Sprintf("--cpus=%f", p.cfg.CPUs),
		fmt.Sprintf("--memory=%d", p.cfg.MemoryBytes),
		fmt.Sprintf("--memory-swap=%d", p.cfg.MemoryBytes),
		fmt.Sprintf("--pids-limit=%d", p.cfg.PidsLimit),
		"-v", fmt.Sprintf("%s:/snapshot:ro", lease.SnapshotDir),
		"-v", fmt.Sprintf("%s:/work:rw", lease.WorkDir),
		"-v", fmt.Sprintf("%s:/tmp:rw", lease.TmpDir),
		"-v", fmt.Sprintf("%s:/cache:rw", lease.CacheDir),
		"-w", "/work",
		"--entrypoint=",
		"-e", "GOCACHE=/cache/go-build",
		"-e", "GOPATH=/cache/go",
		"-e", "GOMODCACHE=/cache/go/pkg/mod",
		"-e", "TMPDIR=/tmp",
		"-e", "GOPROXY=off",
		"-e", "GOTOOLCHAIN=local",
		"-e", "GOVCS=off",
		"-e", "GOWORK=off",
		"-e", "GOENV=off",
		"-e", "HOME=/tmp",
		"-e", "PATH=/usr/local/go/bin:/usr/bin:/bin",
		p.cfg.ImageDigest,
	)
	podmanArgs = append(podmanArgs, args...)

	cmd := exec.CommandContext(ctx, p.cfg.BinaryPath, podmanArgs...)

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

		if ctx.Err() == context.DeadlineExceeded {
			isTimeout = true
			exitCode = 124
		}

		stderrStr := stderrCollector.String()
		lowerStderr := strings.ToLower(stderrStr)
		if strings.Contains(lowerStderr, "no space left on device") || strings.Contains(lowerStderr, "enospc") {
			isDiskFull = true
		}
		if exitCode == 137 || strings.Contains(lowerStderr, "out of memory") || strings.Contains(lowerStderr, "killed") {
			isOOM = true
		}
	}

	result := &StageResult{
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

	return result, nil
}

// StopAndKillJobContainers terminates all containers associated with the job ID
// using an independent cleanup timeout.
func (p *PodmanBackend) StopAndKillJobContainers(ctx context.Context, jobID string) error {
	cleanCtx, cancel := context.WithTimeout(context.Background(), p.cfg.TimeoutClean)
	defer cancel()

	// List containers labeled with this job ID
	listCmd := exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "ps", "-a", "-q", "--filter", fmt.Sprintf("label=pr-review.job_id=%s", jobID))
	var out bytes.Buffer
	listCmd.Stdout = &out
	if err := listCmd.Run(); err != nil {
		return fmt.Errorf("failed to list job containers: %w", err)
	}

	cids := strings.Fields(strings.TrimSpace(out.String()))
	if len(cids) == 0 {
		return nil
	}

	// Kill and remove
	for _, cid := range cids {
		killCmd := exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "kill", cid)
		_ = killCmd.Run()
		rmCmd := exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "rm", "-f", cid)
		_ = rmCmd.Run()
	}

	return nil
}

// ReconcileOrphanContainers cleans up lingering containers from crashed or previous jobs.
func (p *PodmanBackend) ReconcileOrphanContainers(ctx context.Context) error {
	cleanCtx, cancel := context.WithTimeout(ctx, p.cfg.TimeoutClean)
	defer cancel()

	listCmd := exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "ps", "-a", "-q", "--filter", "label=pr-review.job_id")
	var out bytes.Buffer
	listCmd.Stdout = &out
	if err := listCmd.Run(); err != nil {
		return fmt.Errorf("failed listing orphan containers: %w", err)
	}

	cids := strings.Fields(strings.TrimSpace(out.String()))
	for _, cid := range cids {
		_ = exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "kill", cid).Run()
		_ = exec.CommandContext(cleanCtx, p.cfg.BinaryPath, "rm", "-f", cid).Run()
	}

	return nil
}

// In-loop command execution (Phase 06-03, D-03, podman parity).
//
// InLoopExecCommand runs one matrix-constrained build/test command inside the
// writable /work mount of an existing verification container via podman exec.
// It applies the IDENTICAL element-level gate as InLoopCommand (empty,
// shell-metacharacter, and traversal rejection with the same error strings)
// plus the shared matrix-head allow-list, then restricts argv to the union
// of every known matrix row (per-project binding is impossible here: the
// container side has no workDir to detect from, so the strictest
// workDir-free gate is the full known-good set). No shell wrapper anywhere
// in the exec path. A container exit 127 maps to the same
// ErrInLoopToolchainMissing unavailable shape as a missing direct binary —
// never to a failed verdict. Inner pass is advisory only.
func (r *Runner) InLoopExecCommand(ctx context.Context, containerID string, argv []string, timeout time.Duration, maxOutputBytes int) (string, string, int, error) {
	if strings.TrimSpace(containerID) == "" {
		return "", "", -1, fmt.Errorf("in-loop exec requires a container ID")
	}
	if err := validateInLoopArgv(argv); err != nil {
		return "", "", -1, err
	}
	if !inLoopHeadAllowed(argv[0]) {
		return "", "", -1, fmt.Errorf("%s: %q", inLoopMatrixMismatch, strings.Join(argv, " "))
	}
	if !inLoopArgvAllowed(argv, inLoopAllowedUnion()) {
		return "", "", -1, fmt.Errorf("%s: %q", inLoopMatrixMismatch, strings.Join(argv, " "))
	}
	if err := ctx.Err(); err != nil {
		return "", "", -1, err
	}
	binary := "podman"
	if r != nil && r.Backend != nil && strings.TrimSpace(r.Backend.cfg.BinaryPath) != "" {
		binary = r.Backend.cfg.BinaryPath
	}
	if _, err := exec.LookPath(binary); err != nil {
		return "", "", 127, fmt.Errorf("%w: podman binary %q is missing on PATH (in-loop exec unavailable)", ErrInLoopToolchainMissing, binary)
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultInLoopMaxOutputBytes
	}

	execArgs := []string{"exec", "-w", "/work", containerID}
	execArgs = append(execArgs, argv...)

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, binary, execArgs...)
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
		if execCtx.Err() == context.DeadlineExceeded {
			return stdout, stderr, 124, execCtx.Err()
		}
		if exitCode == 127 {
			return stdout, stderr, 127, fmt.Errorf("%w: %q is missing in container %q (exit 127, in-loop command unavailable)", ErrInLoopToolchainMissing, argv[0], containerID)
		}
		return stdout, stderr, exitCode, nil
	}
	return stdout, stderr, 0, nil
}

// inLoopAllowedUnion is the full known-good argv set across every project
// matrix (build plus test rows only; install/network rows excluded). The
// podman backend gates against it because exec has no workDir to detect a
// single project from. inloop_test.go pins that every detectDirectProject
// row is a member, so the union cannot drift from the matrix.
func inLoopAllowedUnion() [][]string {
	return [][]string{
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"go", "test", "-race", "./..."},
		{"poetry", "run", "pytest"},
		{"pytest"},
		{"python3", "-m", "pytest"},
		{"python", "-m", "pytest"},
		{"python3", "-m", "unittest"},
		{"python", "-m", "unittest"},
		{"pnpm", "test"},
		{"yarn", "test"},
		{"npm", "test"},
		{"cargo", "test"},
		{"mvn", "-q", "test"},
		{"gradle", "test"},
		{"make", "test"},
	}
}

// inLoopHeadAllowed reports whether head is a binary appearing in the shared
// matrix union (derived, never a separate literal list, so both backends
// share identical gate strings by construction).
func inLoopHeadAllowed(head string) bool {
	for _, row := range inLoopAllowedUnion() {
		if len(row) > 0 && row[0] == head {
			return true
		}
	}
	return false
}
