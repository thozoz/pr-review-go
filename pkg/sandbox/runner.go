package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type ExecutionResult struct {
	Command  string        `json:"command"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	Passed   bool          `json:"passed"`
}

type VerificationReport struct {
	WorkspaceDir string            `json:"workspace_dir"`
	DetectedType string            `json:"detected_type"` // e.g. "go", "node", "python", "rust"
	Results      []ExecutionResult `json:"results"`
	Summary      string            `json:"summary"`
}

type Runner struct {
	Timeout time.Duration
}

func NewRunner(timeout time.Duration) *Runner {
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	return &Runner{
		Timeout: timeout,
	}
}

// PrepareWorkspace clones the repository and checks out the specific PR commit/branch
func (r *Runner) PrepareWorkspace(ctx context.Context, cloneURL, headRef, headSHA string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "pr-review-sandbox-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create sandbox dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	// Shallow clone branch
	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", headRef, cloneURL, tmpDir)
	var cloneErr bytes.Buffer
	cloneCmd.Stderr = &cloneErr
	if err := cloneCmd.Run(); err != nil {
		// Fallback: clone without branch and checkout SHA
		_ = os.RemoveAll(tmpDir)
		_ = os.MkdirAll(tmpDir, 0755)

		initCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", cloneURL, tmpDir)
		if err := initCmd.Run(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("git clone failed: %v, stderr: %s", err, cloneErr.String())
		}

		coCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "checkout", headSHA)
		if err := coCmd.Run(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("failed to checkout %s: %w", headSHA, err)
		}
	}

	return tmpDir, cleanup, nil
}

// VerifyProject detects project type and executes compilation/tests
func (r *Runner) VerifyProject(ctx context.Context, dir string) (*VerificationReport, error) {
	report := &VerificationReport{
		WorkspaceDir: dir,
		Results:      make([]ExecutionResult, 0),
	}

	// 1. Detect environment
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		report.DetectedType = "go"
		r.runGoVerification(ctx, dir, report)
	} else if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		report.DetectedType = "node"
		r.runNodeVerification(ctx, dir, report)
	} else if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		report.DetectedType = "rust"
		r.runRustVerification(ctx, dir, report)
	} else if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil ||
		fileExists(filepath.Join(dir, "requirements.txt")) {
		report.DetectedType = "python"
		r.runPythonVerification(ctx, dir, report)
	} else {
		report.DetectedType = "generic"
		report.Summary = "No recognized build/test configuration found."
	}

	return report, nil
}

func (r *Runner) executeCommand(ctx context.Context, dir, name string, args ...string) ExecutionResult {
	cmdCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	passed := true
	if err != nil {
		passed = false
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return ExecutionResult{
		Command:  fmt.Sprintf("%s %v", name, args),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: duration,
		Passed:   passed,
	}
}

func (r *Runner) runGoVerification(ctx context.Context, dir string, report *VerificationReport) {
	// Build test
	buildRes := r.executeCommand(ctx, dir, "go", "build", "./...")
	report.Results = append(report.Results, buildRes)

	// Unit test
	testRes := r.executeCommand(ctx, dir, "go", "test", "-race", "./...")
	report.Results = append(report.Results, testRes)

	if !buildRes.Passed {
		report.Summary = "FAILED: `go build ./...` failed to compile."
	} else if !testRes.Passed {
		report.Summary = "FAILED: `go test` reported test failures."
	} else {
		report.Summary = "SUCCESS: Project builds and all unit tests pass cleanly."
	}
}

func (r *Runner) runNodeVerification(ctx context.Context, dir string, report *VerificationReport) {
	// npm test if package.json has scripts.test
	testRes := r.executeCommand(ctx, dir, "npm", "test", "--if-present")
	report.Results = append(report.Results, testRes)
	if !testRes.Passed {
		report.Summary = "FAILED: npm test failed."
	} else {
		report.Summary = "SUCCESS: Node verification passed."
	}
}

func (r *Runner) runRustVerification(ctx context.Context, dir string, report *VerificationReport) {
	checkRes := r.executeCommand(ctx, dir, "cargo", "check")
	report.Results = append(report.Results, checkRes)
	if !checkRes.Passed {
		report.Summary = "FAILED: cargo check failed."
	} else {
		report.Summary = "SUCCESS: Rust cargo check passed."
	}
}

func (r *Runner) runPythonVerification(ctx context.Context, dir string, report *VerificationReport) {
	// Quick pytest check if pytest exists
	testRes := r.executeCommand(ctx, dir, "pytest", "-q")
	report.Results = append(report.Results, testRes)
	if !testRes.Passed {
		report.Summary = "FAILED: pytest reported issues."
	} else {
		report.Summary = "SUCCESS: Python tests passed."
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
