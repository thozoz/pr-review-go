package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	WorkspaceDir   string            `json:"workspace_dir"`
	DetectedType   string            `json:"detected_type"` // e.g. "go", "node", "python", "rust"
	Results        []ExecutionResult `json:"results"`
	Summary        string            `json:"summary"`
	CustomRules    string            `json:"custom_rules,omitempty"` // Content of AGENTS.md, copilot-instructions.md, etc.
	RulesSource    string            `json:"rules_source,omitempty"` // Filename of discovered rules
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
	if cloneURL == "" {
		return "", nil, fmt.Errorf("clone URL is empty")
	}
	if headSHA == "" {
		return "", nil, fmt.Errorf("head SHA is empty")
	}

	tmpDir, err := os.MkdirTemp("", "pr-review-sandbox-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create sandbox dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	// Try shallow clone with branch first (fastest)
	if headRef != "" {
		cloneCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", headRef, cloneURL, tmpDir)
		var cloneErr bytes.Buffer
		cloneCmd.Stderr = &cloneErr
		if err := cloneCmd.Run(); err == nil {
			// Verify we have the correct commit
			verifyCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "rev-parse", "HEAD")
			var verifyOut bytes.Buffer
			verifyCmd.Stdout = &verifyOut
			if err := verifyCmd.Run(); err == nil {
				currentSHA := strings.TrimSpace(verifyOut.String())
				if strings.HasPrefix(headSHA, currentSHA) || strings.HasPrefix(currentSHA, headSHA) {
					return tmpDir, cleanup, nil
				}
			}
		}
		// Fall through to fallback
		_ = os.RemoveAll(tmpDir)
		_ = os.MkdirAll(tmpDir, 0755)
	}

	// Fallback: clone with more depth and checkout specific SHA
	// Use --depth 100 to get enough history for the SHA
	initCmd := exec.CommandContext(ctx, "git", "clone", "--depth", "100", cloneURL, tmpDir)
	var initErr bytes.Buffer
	initCmd.Stderr = &initErr
	if err := initCmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone failed: %v, stderr: %s", err, initErr.String())
	}

	// Try to checkout the specific commit
	coCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "checkout", headSHA)
	var coErr bytes.Buffer
	coCmd.Stderr = &coErr
	if err := coCmd.Run(); err != nil {
		// If checkout fails, try fetching more history
		fetchCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "fetch", "--depth=1000", "origin", headSHA)
		if fetchErr := fetchCmd.Run(); fetchErr == nil {
			retryCoCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "checkout", headSHA)
			if retryErr := retryCoCmd.Run(); retryErr == nil {
				return tmpDir, cleanup, nil
			}
		}
		cleanup()
		return "", nil, fmt.Errorf("failed to checkout %s: %w, stderr: %s", headSHA, err, coErr.String())
	}

	return tmpDir, cleanup, nil
}

// VerifyProject detects project type, executes compilation/tests, and extracts custom instruction rules
func (r *Runner) VerifyProject(ctx context.Context, dir string) (*VerificationReport, error) {
	report := &VerificationReport{
		WorkspaceDir: dir,
		Results:      make([]ExecutionResult, 0),
	}

	// Read repository custom rules if present
	r.extractCustomRules(dir, report)

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
		} else if cmdCtx.Err() == context.DeadlineExceeded {
			exitCode = 124
			stderr.WriteString("\n[ERROR] Command timed out after " + r.Timeout.String())
		} else {
			exitCode = 1
		}
	}

	// Limit output size to prevent memory issues with large test outputs
	const maxOutputSize = 100 * 1024 // 100 KB
	stdoutStr := stdout.String()
	stderrStr := stderr.String()
	if len(stdoutStr) > maxOutputSize {
		stdoutStr = stdoutStr[:maxOutputSize] + "\n... [stdout truncated, exceeded " + fmt.Sprintf("%d KB", maxOutputSize/1024) + "] ..."
	}
	if len(stderrStr) > maxOutputSize {
		stderrStr = stderrStr[:maxOutputSize] + "\n... [stderr truncated, exceeded " + fmt.Sprintf("%d KB", maxOutputSize/1024) + "] ..."
	}

	return ExecutionResult{
		Command:  fmt.Sprintf("%s %v", name, args),
		Stdout:   stdoutStr,
		Stderr:   stderrStr,
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

func (r *Runner) extractCustomRules(dir string, report *VerificationReport) {
	// Standard instruction & guidelines file candidates in priority order
	candidates := []string{
		".github/copilot-instructions.md",
		".github/instructions.md",
		".github/CONTRIBUTING.md",
		"CONTRIBUTING.md",
		"contributing.md",
		"AGENTS.md",
		"agents.md",
		"CLAUDE.md",
		"claude.md",
		".cursorrules",
		"DEVELOPMENT.md",
		"DEVELOPING.md",
		"STYLEGUIDE.md",
		"REVIEW_GUIDELINES.md",
	}

	for _, rel := range candidates {
		target := filepath.Join(dir, rel)
		data, err := os.ReadFile(target)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			content := string(data)
			if len(content) > 15000 {
				content = content[:15000] + "\n...[instructions truncated]..."
			}
			report.CustomRules = content
			report.RulesSource = rel
			return
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
