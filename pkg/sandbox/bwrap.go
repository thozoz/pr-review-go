package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrBwrapUnavailable is the typed sentinel returned when bubblewrap/userns
// isolation cannot be provided. Callers MUST map it to an honest
// reduced-guarantee report (StatusUnavailable carrying ReducedGuaranteeLabel),
// never to StatusPassed and never to a silent pass.
var ErrBwrapUnavailable = errors.New("bubblewrap userns isolation is unavailable")

// ReducedGuaranteeLabel is the exact honest-degradation label reported when
// userns isolation is unavailable (D-11). Wording is load-bearing: tests and
// status rendering assert this literal.
const ReducedGuaranteeLabel = "REDUCED-GUARANTEE: userns unavailable, network isolation not enforced"

// BwrapGatewaySocketPath is the in-sandbox path of the per-job dependency
// gateway socket. It is bound ONLY in stage 1 and MUST be absent in stage 2.
const BwrapGatewaySocketPath = "/run/sandbox/gateway.sock"

// BwrapStage1Goproxy is the GOPROXY value inside stage 1: the loopback relay
// that forwards module fetches to the per-job gateway UDS (same
// loopback-relay helper pattern as dependencies.go). Stage 2 uses GOPROXY=off.
const BwrapStage1Goproxy = "http://localhost:8080"

// ValidateBwrapFetchArgv enforces the fixed-argv builder contract: stage 1
// executes ONLY the constant triple {go, mod, download}. Any caller-supplied
// flags, URLs, alternate tools (curl, wget), or mutating commands (go get,
// go mod download with extra flags) are rejected. Server env is never
// inherited; there is no caller flag parameter anywhere in the builders.
func ValidateBwrapFetchArgv(argv []string) error {
	if len(argv) == 3 && argv[0] == "go" && argv[1] == "mod" && argv[2] == "download" {
		return nil
	}
	return fmt.Errorf("bwrap stage-1 fetch argv rejected (fixed argv is exactly {go mod download}): %q", strings.Join(argv, " "))
}

// buildStage1Argv returns the fixed stage-1 (fetch) bwrap argv: unprivileged
// userns+net+pid isolation with the per-job gateway socket bound at
// /run/sandbox/gateway.sock and GOPROXY pointed at the loopback relay,
// executing only the fixed triple `go mod download`.
func buildStage1Argv(workDir, cacheDir, tmpDir, gwSockPath string) []string {
	argv := []string{
		"bwrap",
		"--unshare-user", "--unshare-net", "--unshare-pid",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", workDir, "/work",
		"--bind", cacheDir, "/cache",
		"--bind", tmpDir, "/tmp",
		"--bind", gwSockPath, BwrapGatewaySocketPath,
		"--clearenv",
		"--setenv", "PATH", "/usr/local/go/bin:/usr/bin:/bin",
		"--setenv", "HOME", "/tmp",
		"--setenv", "GOPROXY", BwrapStage1Goproxy,
		"--chdir", "/work",
		"go", "mod", "download",
	}
	return argv
}

// buildStage2Argv returns the fixed stage-2 (execute) bwrap argv: identical
// isolation MINUS every socket bind, PLUS the hermetic Go env. The gateway
// socket path is absent, not merely unused. Takes no socket parameter by
// design so callers cannot smuggle a bind into stage 2.
func buildStage2Argv(workDir, cacheDir, tmpDir string) []string {
	argv := []string{
		"bwrap",
		"--unshare-user", "--unshare-net", "--unshare-pid",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/usr", "/usr",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", workDir, "/work",
		"--bind", cacheDir, "/cache",
		"--bind", tmpDir, "/tmp",
		"--clearenv",
		"--setenv", "PATH", "/usr/local/go/bin:/usr/bin:/bin",
		"--setenv", "HOME", "/tmp",
		"--setenv", "GOPROXY", "off",
		"--setenv", "GOTOOLCHAIN", "local",
		"--setenv", "GOVCS", "off",
		"--setenv", "GOWORK", "off",
		"--setenv", "GOENV", "off",
		"--setenv", "GOCACHE", "/cache/go-build",
		"--setenv", "GOPATH", "/cache/go",
		"--setenv", "GOMODCACHE", "/cache/go/pkg/mod",
		"--setenv", "TMPDIR", "/tmp",
		"--chdir", "/work",
	}
	return argv
}

// buildStage2ExecArgv appends a fixed, gate-checked stage command to the
// stage-2 prefix. Only exact build/test rows are admitted; everything else
// is rejected. Kept unexported: 07-03 wires the runner to these rows.
func buildStage2ExecArgv(workDir, cacheDir, tmpDir string, stageCmd []string) ([]string, error) {
	allowed := [][]string{
		{"go", "build", "./..."},
		{"go", "vet", "./..."},
		{"go", "test", "-race", "./..."},
	}
	ok := false
	for _, row := range allowed {
		if len(stageCmd) == len(row) {
			match := true
			for i := range row {
				if stageCmd[i] != row[i] {
					match = false
					break
				}
			}
			if match {
				ok = true
				break
			}
		}
	}
	if !ok {
		return nil, fmt.Errorf("bwrap stage-2 command rejected (fixed build/test rows only): %q", strings.Join(stageCmd, " "))
	}
	return append(buildStage2Argv(workDir, cacheDir, tmpDir), stageCmd...), nil
}

// RunBwrapStage executes one fixed bwrap stage argv with the podman.go exit
// classification: context deadline maps to exit 124 IsTimeout, ENOSPC output
// maps to IsDiskFull, 137/killed maps to IsOOM. Output is bounded by
// LimitedCollector at 100 KiB per stream. The argv MUST come from the
// builders above; no caller-supplied flags, URLs, or env passthrough.
func RunBwrapStage(ctx context.Context, stageName string, bwrapArgv []string) (*StageResult, error) {
	if len(bwrapArgv) == 0 {
		return nil, fmt.Errorf("bwrap stage argv is empty")
	}
	if bwrapArgv[0] != "bwrap" {
		return nil, fmt.Errorf("bwrap stage argv must start with bwrap")
	}

	cmd := exec.CommandContext(ctx, bwrapArgv[0], bwrapArgv[1:]...)
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
		Command:      strings.Join(bwrapArgv, " "),
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

// NOTE for 07-03 (runner wiring): map ErrBwrapUnavailable to a
// VerificationReport with Status == StatusUnavailable and
// Reason/Summary carrying ReducedGuaranteeLabel. Never StatusPassed,
// never silent. See BwrapUnavailableReport.
var _ = StatusUnavailable

// BwrapUnavailableReport builds the honest-degradation report for the
// bwrap-unavailable path. It always carries StatusUnavailable with the exact
// ReducedGuaranteeLabel, never StatusPassed.
func BwrapUnavailableReport(reason string) *VerificationReport {
	detail := strings.TrimSpace(reason)
	if detail == "" {
		detail = ReducedGuaranteeLabel
	} else if !strings.Contains(detail, ReducedGuaranteeLabel) {
		detail = ReducedGuaranteeLabel + ": " + detail
	}
	return &VerificationReport{
		Status:  StatusUnavailable,
		Reason:  detail,
		Summary: "UNAVAILABLE: " + detail,
		Results: make([]ExecutionResult, 0),
	}
}

// AttestBwrap probes bubblewrap/userns availability for the CT deployment
// target (D-09/D-10/D-11). Steps:
//
//  1. exec.LookPath for the bwrap binary (OS distro package: apt install bubblewrap).
//  2. `bwrap --version` under a 10-second context.
//  3. `unshare -Urn true` under a 10-second context as the userns-plus-net
//     availability probe.
//  4. Best-effort read of /proc/sys/kernel/unprivileged_userns_clone as a
//     secondary signal when present (informational only, never gates).
//
// On ANY probe failure it returns ErrBwrapUnavailable. There is no
// privileged fallback: upstream removed privileged mode, so unprivileged
// user-namespace operation is the only supported shape. Zero new Go module
// dependencies; the only dependency is the OS distro bubblewrap package.
func AttestBwrap(ctx context.Context) error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("%w: bwrap binary not found on PATH: %v", ErrBwrapUnavailable, err)
	}

	verCtx, verCancel := context.WithTimeout(ctx, 10*time.Second)
	defer verCancel()
	verCmd := exec.CommandContext(verCtx, "bwrap", "--version")
	var verOut, verErr bytes.Buffer
	verCmd.Stdout = &verOut
	verCmd.Stderr = &verErr
	if err := verCmd.Run(); err != nil {
		return fmt.Errorf("%w: bwrap --version probe failed: %v (stderr: %s)", ErrBwrapUnavailable, err, strings.TrimSpace(verErr.String()))
	}

	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()
	probeCmd := exec.CommandContext(probeCtx, "unshare", "-Urn", "true")
	var probeOut, probeErr bytes.Buffer
	probeCmd.Stdout = &probeOut
	probeCmd.Stderr = &probeErr
	if err := probeCmd.Run(); err != nil {
		return fmt.Errorf("%w: userns+net probe (unshare -Urn true) failed: %v (stderr: %s)", ErrBwrapUnavailable, err, strings.TrimSpace(probeErr.String()))
	}

	// Secondary signal only: some Debian/Proxmox kernels gate unprivileged
	// userns creation via /proc/sys/kernel/unprivileged_userns_clone.
	// Absent file or unexpected content never fails attestation; the live
	// unshare probe above is authoritative.
	if data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		_ = strings.TrimSpace(string(data))
	}

	return nil
}
