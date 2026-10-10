package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
)

// VerificationStatus represents the explicit, typed status of PR verification (D-15).
type VerificationStatus string

const (
	StatusPassed              VerificationStatus = "passed"
	StatusBuildFailed         VerificationStatus = "build_failed"
	StatusTestFailed          VerificationStatus = "test_failed"
	StatusTimeout             VerificationStatus = "timeout"
	StatusResourceExhausted   VerificationStatus = "resource_exhausted"
	StatusDiskExhausted       VerificationStatus = "disk_exhausted"
	StatusUnavailable         VerificationStatus = "unavailable"
	StatusUnsupportedLanguage  VerificationStatus = "unsupported_language"
	StatusIncomplete          VerificationStatus = "incomplete"
	StatusCancelled           VerificationStatus = "cancelled"
)

var (
	ErrSourceProviderUnavailable = errors.New("remote source preparation is unavailable: host clone is disabled (safe isolation pending Plan 02)")
	ErrInvalidSnapshot           = errors.New("invalid or malformed snapshot")
)

type ExecutionResult struct {
	Command      string        `json:"command"`
	Stdout       string        `json:"stdout"`
	Stderr       string        `json:"stderr"`
	ExitCode     int           `json:"exit_code"`
	Duration     time.Duration `json:"duration"`
	Passed       bool          `json:"passed"`
	Truncated    bool          `json:"truncated,omitempty"`
	DroppedBytes int64         `json:"dropped_bytes,omitempty"`
}

type VerificationReport struct {
	WorkspaceDir string            `json:"workspace_dir"`
	DetectedType string            `json:"detected_type"` // e.g. "go", "node", "python", "rust"
	Results      []ExecutionResult `json:"results"`
	Summary      string            `json:"summary"`
	CustomRules  string            `json:"custom_rules,omitempty"`
	RulesSource  string            `json:"rules_source,omitempty"`

	// Typed status and diagnostic fields (D-15)
	Status       VerificationStatus `json:"status"`
	Reason       string             `json:"reason,omitempty"`
	CommitSHA    string             `json:"commit_sha,omitempty"`
	FailedStage  string             `json:"failed_stage,omitempty"`
	Truncated    bool               `json:"truncated,omitempty"`
	DroppedBytes int64              `json:"dropped_bytes,omitempty"`
}

// Snapshot owns the immutable repository snapshot and its complete identity manifest.
type Snapshot struct {
	CommitSHA        string            `json:"commit_sha"`
	SourceDir        string            `json:"source_dir"`
	Manifest         map[string]string `json:"manifest,omitempty"` // path -> sha256
	IsIncomplete     bool              `json:"is_incomplete,omitempty"`
	IncompleteReason string            `json:"incomplete_reason,omitempty"`
	hasPermit        bool
}

// Validate ensures the snapshot has a complete, valid commit SHA and a clean source tree.
func (s *Snapshot) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: snapshot is nil", ErrInvalidSnapshot)
	}

	// Full commit comparison: reject empty, short prefixes, or non-hex characters (SAFE-01)
	shaLen := len(s.CommitSHA)
	if shaLen != 40 && shaLen != 64 {
		return fmt.Errorf("%w: commit SHA must be 40 or 64 hex characters (got length %d: %q)", ErrInvalidSnapshot, shaLen, s.CommitSHA)
	}
	for i := 0; i < shaLen; i++ {
		c := s.CommitSHA[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%w: commit SHA contains invalid character %q", ErrInvalidSnapshot, string(c))
		}
	}

	if s.SourceDir == "" {
		return fmt.Errorf("%w: source directory cannot be empty", ErrInvalidSnapshot)
	}

	cleanDir, err := filepath.Abs(s.SourceDir)
	if err != nil {
		return fmt.Errorf("%w: invalid source directory: %v", ErrInvalidSnapshot, err)
	}
	s.SourceDir = cleanDir

	fi, err := os.Stat(s.SourceDir)
	if err != nil {
		return fmt.Errorf("%w: source directory stat failed: %v", ErrInvalidSnapshot, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: source path %q is not a directory", ErrInvalidSnapshot, s.SourceDir)
	}

	// Verify entries inside SourceDir for escaping symlinks and dangerous special files
	err = filepath.Walk(s.SourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		mode := info.Mode()
		if mode&os.ModeDevice != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 {
			return fmt.Errorf("%w: forbidden special file %q", ErrInvalidSnapshot, path)
		}
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), target)
			}
			resolvedClean := filepath.Clean(resolved)
			rel, err := filepath.Rel(s.SourceDir, resolvedClean)
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("%w: escaping symlink %q -> %q", ErrInvalidSnapshot, path, target)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// SourceProvider is the typed seam for repository source retrieval.
type SourceProvider interface {
	PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error)
}

type Runner struct {
	Timeout        time.Duration
	Config         *config.Config
	Backend        *PodmanBackend
	SlotManager    SlotManager
	SourceProvider SourceProvider
	DepPreparer    DependencyPreparer
	// Direct executes verification without containers when
	// Config.SandboxMode == "direct". Nil in podman mode.
	Direct *DirectRunner
}

func NewRunner(timeout time.Duration) *Runner {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Runner{
		Timeout: timeout,
	}
}

func NewRunnerWithConfig(cfg *config.Config, backend *PodmanBackend, sm SlotManager) *Runner {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.SandboxTimeoutExecution > 0 {
		timeout = cfg.SandboxTimeoutExecution
	}
	r := &Runner{
		Timeout:     timeout,
		Config:      cfg,
		Backend:     backend,
		SlotManager: sm,
	}
	if backend != nil && sm != nil {
		r.SourceProvider = NewPublicGitSource(backend, sm, cfg)
		r.DepPreparer = NewDependencyPreparer(backend, cfg)
	}
	if cfg != nil && cfg.SandboxMode == config.SandboxModeDirect {
		r.Direct = DirectRunnerForConfig(cfg)
	}
	return r
}

// SetSourceProvider configures the source provider for snapshot retrieval.
func (r *Runner) SetSourceProvider(sp SourceProvider) {
	r.SourceProvider = sp
}

// NewPlatformRunner creates a Runner with the appropriate source provider for the current environment.
// If backend and slot manager are available, it uses PublicGitSource.
// On non-execution platforms (or when ghClient is provided without container backend),
// it configures APISnapshotSource for read-only snapshots.
func NewPlatformRunner(cfg *config.Config, ghClient *github.Client, backend *PodmanBackend, sm SlotManager) *Runner {
	r := NewRunnerWithConfig(cfg, backend, sm)
	if r.SourceProvider == nil && ghClient != nil {
		r.SourceProvider = NewAPISnapshotSource(ghClient, ghClient, 0, false)
	}
	return r
}

func (r *Runner) prepTimeout() time.Duration {
	if r.Config != nil && r.Config.SandboxTimeoutPrep > 0 {
		return r.Config.SandboxTimeoutPrep
	}
	return 3 * time.Minute
}

func (r *Runner) executionTimeout() time.Duration {
	if r.Config != nil && r.Config.SandboxTimeoutExecution > 0 {
		return r.Config.SandboxTimeoutExecution
	}
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 5 * time.Minute
}

func (r *Runner) cleanupTimeout() time.Duration {
	if r.Config != nil && r.Config.SandboxTimeoutCleanup > 0 {
		return r.Config.SandboxTimeoutCleanup
	}
	return 15 * time.Second
}

// PrepareSnapshot retrieves a verified source snapshot using the configured SourceProvider.
func (r *Runner) PrepareSnapshot(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if r.SourceProvider == nil {
		return nil, nil, ErrSourceProviderUnavailable
	}

	gate := GateFromContext(ctx)
	var releasePermit func()
	if gate != nil && !HasActivePermit(ctx) {
		rel, err := gate.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		releasePermit = rel
	}

	prepCtx := ctx
	if releasePermit != nil {
		prepCtx = WithActivePermit(ctx)
	}

	snap, cleanup, err := r.SourceProvider.PrepareSource(prepCtx, cloneURL, headRef, headSHA)
	if err != nil {
		if releasePermit != nil {
			releasePermit()
		}
		return nil, nil, err
	}

	if snap != nil && releasePermit != nil {
		snap.hasPermit = true
	}

	var once sync.Once
	wrappedCleanup := func() {
		once.Do(func() {
			if cleanup != nil {
				cleanup()
			}
			if releasePermit != nil {
				releasePermit()
			}
		})
	}

	return snap, wrappedCleanup, nil
}

// PrepareWorkspace adapts the legacy workspace preparation method to the safe snapshot model.
func (r *Runner) PrepareWorkspace(ctx context.Context, cloneURL, headRef, headSHA string) (string, func(), error) {
	snap, cleanup, err := r.PrepareSnapshot(ctx, cloneURL, headRef, headSHA)
	if err != nil {
		return "", nil, err
	}
	return snap.SourceDir, cleanup, nil
}

// VerifyProject inspects the project, enforces custom rule limits, and executes Go verification
// using container isolation only. Host command execution is prohibited.
func (r *Runner) VerifyProject(ctx context.Context, dir string) (*VerificationReport, error) {
	report := &VerificationReport{
		WorkspaceDir: dir,
		Results:      make([]ExecutionResult, 0),
	}

	r.extractCustomRules(dir, report)

	// Direct mode verifies every project type in place on the trusted host
	// without containers, slots, or attestation.
	if r.Direct != nil {
		snapshot := &Snapshot{
			CommitSHA: computeDirSHA(dir),
			SourceDir: dir,
		}
		return r.RunSnapshot(ctx, snapshot)
	}

	// 1. Detect project environment
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		report.DetectedType = "go"

		// Direct mode tries the build honestly against the local module
		// cache (GOPROXY=off) instead of failing closed here.
		if r.Direct == nil && hasUnvendoredDependencies(dir) && r.DepPreparer == nil {
			report.Status = StatusIncomplete
			report.Reason = "external dependencies require network gateway (Plan 04)"
			report.Summary = "INCOMPLETE: External dependencies require network gateway."
			return report, nil
		}

		// On non-Linux platforms (macOS/Windows), container build/test
		// isolation is unavailable (D-02). Direct mode is exempt: it needs
		// only a Go toolchain, and the host is trusted by configuration.
		if r.Direct == nil && runtime.GOOS != "linux" {
			report.Status = StatusUnavailable
			report.Reason = fmt.Sprintf("isolated build/test verification is unavailable on %s; Linux rootless container isolation required (D-02)", runtime.GOOS)
			report.Summary = fmt.Sprintf("UNAVAILABLE: Verification is unavailable on %s (Linux container isolation required).", runtime.GOOS)
			return report, nil
		}

		// Container isolation is required for Go PR execution (D-01)
		if r.Direct == nil && (r.Backend == nil || r.SlotManager == nil) {
			report.Status = StatusUnavailable
			report.Reason = "container isolation backend or storage slot not configured"
			report.Summary = "SKIPPED: Verification skipped due to missing container isolation."
			return report, nil
		}

		// Prepare a sealed snapshot from the directory
		sha := computeDirSHA(dir)
		snapshot := &Snapshot{
			CommitSHA: sha,
			SourceDir: dir,
		}

		return r.RunSnapshot(ctx, snapshot)
	} else if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		report.DetectedType = "node"
		report.Status = StatusUnsupportedLanguage
		report.Reason = "Node verification is unsupported in Phase 1 (D-03)"
		report.Summary = "SKIPPED: Verification skipped due to missing container isolation."
	} else if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err == nil {
		report.DetectedType = "rust"
		report.Status = StatusUnsupportedLanguage
		report.Reason = "Rust verification is unsupported in Phase 1 (D-03)"
		report.Summary = "SKIPPED: Verification skipped due to missing container isolation."
	} else if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil ||
		fileExists(filepath.Join(dir, "requirements.txt")) {
		report.DetectedType = "python"
		report.Status = StatusUnsupportedLanguage
		report.Reason = "Python verification is unsupported in Phase 1 (D-03)"
		report.Summary = "SKIPPED: Verification skipped due to missing container isolation."
	} else {
		report.DetectedType = "generic"
		report.Status = StatusUnsupportedLanguage
		report.Reason = "No recognized build/test configuration found"
		report.Summary = "No recognized build/test configuration found."
	}

	return report, nil
}

// RunSnapshot executes a verified snapshot through the protected Podman tracer path.
func (r *Runner) RunSnapshot(ctx context.Context, snapshot *Snapshot) (*VerificationReport, error) {
	report := &VerificationReport{
		Results: make([]ExecutionResult, 0),
	}

	alreadyHeld := HasActivePermit(ctx) || (snapshot != nil && snapshot.hasPermit)
	gate := GateFromContext(ctx)
	if !alreadyHeld && gate != nil {
		rel, err := gate.Acquire(ctx)
		if err != nil {
			report.Status = StatusCancelled
			report.Reason = err.Error()
			report.Summary = fmt.Sprintf("CANCELLED: %v", err)
			return report, err
		}
		defer rel()
	}

	if snapshot != nil {
		report.WorkspaceDir = snapshot.SourceDir
		report.CommitSHA = snapshot.CommitSHA
		r.extractCustomRules(snapshot.SourceDir, report)
	}

	if snapshot.IsIncomplete {
		report.Status = StatusIncomplete
		report.Reason = snapshot.IncompleteReason
		report.Summary = fmt.Sprintf("INCOMPLETE: %s", snapshot.IncompleteReason)
		return report, nil
	}

	if err := snapshot.Validate(); err != nil {
		report.Status = StatusUnavailable
		report.Reason = err.Error()
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	// Direct mode skips slot leasing, attestation, and containers: the
	// snapshot source directory is verified in place on the trusted host.
	if r.Direct != nil {
		return r.runDirectSnapshot(ctx, snapshot)
	}

	// On non-Linux platforms (macOS/Windows), build/test isolation is unavailable (D-02)
	if runtime.GOOS != "linux" {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("isolated build/test verification is unavailable on %s; Linux rootless container isolation required (D-02)", runtime.GOOS)
		report.Summary = fmt.Sprintf("UNAVAILABLE: Verification is unavailable on %s (Linux container isolation required).", runtime.GOOS)
		return report, nil
	}

	if r.SlotManager == nil {
		report.Status = StatusUnavailable
		report.Reason = "storage slot manager is unconfigured"
		report.Summary = "UNAVAILABLE: storage slot manager is unconfigured"
		return report, nil
	}

	if r.Backend == nil {
		report.Status = StatusUnavailable
		report.Reason = "container backend is unconfigured"
		report.Summary = "UNAVAILABLE: container backend is unconfigured"
		return report, nil
	}

	// 1. Acquire exclusive slot lease
	jobID := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%d", snapshot.CommitSHA, time.Now().UnixNano()))))[:12]
	lease, err := r.SlotManager.AcquireSlot(ctx, jobID)
	if err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("failed to acquire storage slot: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	// Cleanup order (D-12, SAFE-01 concurrency):
	// Stop/kill named job containers with independent 15s cleanup context,
	// verify all writers stopped, then safely remove files before releasing lease.
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), r.cleanupTimeout())
		defer cancel()
		_ = r.Backend.StopAndKillJobContainers(cleanCtx, jobID)
		_ = r.SlotManager.ReleaseSlot(lease)
	}()

	// 2. Populate sealed snapshot directory and writable work directory
	if snapshot.SourceDir != lease.SnapshotDir {
		if err := copyDirectory(snapshot.SourceDir, lease.SnapshotDir); err != nil {
			report.Status = StatusUnavailable
			report.Reason = fmt.Sprintf("failed to populate snapshot directory: %v", err)
			report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
			return report, nil
		}
	}
	if err := copyDirectory(snapshot.SourceDir, lease.WorkDir); err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("failed to populate work directory: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	// 3. Attest rootless Podman and cgroup-v2 limits before admitting PR code (D-01, D-06)
	if err := r.Backend.Attest(ctx); err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("runtime capability attestation failed: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	// 4. Validate go.mod replacements remain in-root (SAFE-01 encoding, D-05)
	if err := ValidateGoMod(lease.WorkDir); err != nil {
		report.Status = StatusIncomplete
		report.Reason = err.Error()
		report.Summary = fmt.Sprintf("INCOMPLETE: %v", err)
		return report, nil
	}

	// 5. Dependency preparation stage (D-05, D-06, D-07, D-12)
	if r.DepPreparer != nil {
		prepCtx, cancelPrep := context.WithTimeout(ctx, r.prepTimeout())
		defer cancelPrep()

		prepRes, err := r.DepPreparer.PrepareDependencies(prepCtx, jobID, lease)
		if err != nil {
			report.Status = StatusUnavailable
			report.Reason = fmt.Sprintf("dependency preparation error: %v", err)
			report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
			return report, nil
		}
		if prepRes != nil && !prepRes.Passed {
			report.FailedStage = "prep"
			if prepRes.IsTimeout {
				report.Status = StatusTimeout
				report.Reason = "dependency preparation exceeded execution timeout"
				report.Summary = "TIMEOUT: dependency preparation timed out."
			} else if prepRes.IsDiskFull {
				report.Status = StatusDiskExhausted
				report.Reason = "filesystem disk quota exceeded during dependency preparation"
				report.Summary = "DISK_EXHAUSTED: dependency preparation failed with ENOSPC."
			} else if prepRes.IsOOM {
				report.Status = StatusResourceExhausted
				report.Reason = "memory or cgroup limit exceeded during dependency preparation"
				report.Summary = "RESOURCE_EXHAUSTED: dependency preparation exceeded memory limits."
			} else if prepRes.IsIncomplete || prepRes.Command == "INCOMPLETE" {
				report.Status = StatusIncomplete
				report.Reason = strings.TrimSpace(prepRes.Stderr)
				if report.Reason == "" {
					report.Reason = "dependency requirement incomplete"
				}
				report.Summary = fmt.Sprintf("INCOMPLETE: %s", report.Reason)
			} else {
				report.Status = StatusBuildFailed
				report.Reason = strings.TrimSpace(prepRes.Stderr)
				if report.Reason == "" {
					report.Reason = "dependency preparation failed"
				}
				report.Summary = fmt.Sprintf("FAILED: dependency preparation failed: %s", report.Reason)
			}
			return report, nil
		}
	}

	// 6. Combined 5-minute deadline for build and race-test stages (D-06)
	execCtx, cancelExec := context.WithTimeout(ctx, r.executionTimeout())
	defer cancelExec()

	// Stage 1: Build (go build ./...)
	buildRes, err := r.Backend.RunStage(execCtx, jobID, "build", lease, []string{"go", "build", "./..."})
	if err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("build stage execution error: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	report.Results = append(report.Results, ExecutionResult{
		Command:      buildRes.Command,
		Stdout:       buildRes.Stdout,
		Stderr:       buildRes.Stderr,
		ExitCode:     buildRes.ExitCode,
		Duration:     buildRes.Duration,
		Passed:       buildRes.Passed,
		Truncated:    buildRes.Truncated,
		DroppedBytes: buildRes.DroppedBytes,
	})
	if buildRes.Truncated {
		report.Truncated = true
		report.DroppedBytes += buildRes.DroppedBytes
	}

	if !buildRes.Passed {
		report.FailedStage = "build"
		if buildRes.IsTimeout {
			report.Status = StatusTimeout
			report.Reason = "build stage exceeded execution timeout"
			report.Summary = "TIMEOUT: `go build ./...` timed out."
		} else if buildRes.IsDiskFull {
			report.Status = StatusDiskExhausted
			report.Reason = "filesystem disk quota exceeded during build"
			report.Summary = "DISK_EXHAUSTED: `go build ./...` failed with ENOSPC."
		} else if buildRes.IsOOM {
			report.Status = StatusResourceExhausted
			report.Reason = "memory or cgroup limit exceeded during build"
			report.Summary = "RESOURCE_EXHAUSTED: `go build ./...` exceeded memory limits."
		} else {
			report.Status = StatusBuildFailed
			report.Reason = "go build ./... failed to compile"
			report.Summary = "FAILED: `go build ./...` failed to compile."
		}
		return report, nil
	}

	// Stage 2: Unit test with race detector (go test -race ./...)
	testRes, err := r.Backend.RunStage(execCtx, jobID, "test", lease, []string{"go", "test", "-race", "./..."})
	if err != nil {
		report.Status = StatusUnavailable
		report.Reason = fmt.Sprintf("test stage execution error: %v", err)
		report.Summary = fmt.Sprintf("UNAVAILABLE: %v", err)
		return report, nil
	}

	report.Results = append(report.Results, ExecutionResult{
		Command:      testRes.Command,
		Stdout:       testRes.Stdout,
		Stderr:       testRes.Stderr,
		ExitCode:     testRes.ExitCode,
		Duration:     testRes.Duration,
		Passed:       testRes.Passed,
		Truncated:    testRes.Truncated,
		DroppedBytes: testRes.DroppedBytes,
	})
	if testRes.Truncated {
		report.Truncated = true
		report.DroppedBytes += testRes.DroppedBytes
	}

	if !testRes.Passed {
		report.FailedStage = "test"
		if testRes.IsTimeout {
			report.Status = StatusTimeout
			report.Reason = "test stage exceeded execution timeout"
			report.Summary = "TIMEOUT: `go test -race ./...` timed out."
		} else if testRes.IsDiskFull {
			report.Status = StatusDiskExhausted
			report.Reason = "filesystem disk quota exceeded during test"
			report.Summary = "DISK_EXHAUSTED: `go test -race ./...` failed with ENOSPC."
		} else if testRes.IsOOM {
			report.Status = StatusResourceExhausted
			report.Reason = "memory or cgroup limit exceeded during test"
			report.Summary = "RESOURCE_EXHAUSTED: `go test -race ./...` exceeded memory limits."
		} else {
			report.Status = StatusTestFailed
			report.Reason = "go test -race ./... reported test failures"
			report.Summary = "FAILED: `go test` reported test failures."
		}
		return report, nil
	}

	// Both build and test succeeded (D-15)
	report.Status = StatusPassed
	report.Summary = "SUCCESS: Project builds and all unit tests pass cleanly."
	return report, nil
}

// hasUnvendoredDependencies checks if go.mod has require directives while vendor/ is absent.
func hasUnvendoredDependencies(dir string) bool {
	goModPath := filepath.Join(dir, "go.mod")
	f, err := os.Open(goModPath)
	if err != nil {
		return false
	}
	defer f.Close()

	hasRequire := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "require") {
			hasRequire = true
			break
		}
	}

	if !hasRequire {
		return false
	}

	// Check if vendor/modules.txt exists
	vendorModulesPath := filepath.Join(dir, "vendor", "modules.txt")
	_, err = os.Stat(vendorModulesPath)
	return err != nil
}

func computeDirSHA(dir string) string {
	h := sha256.New()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		if f, err := os.Open(path); err == nil {
			_, _ = io.Copy(h, f)
			_ = f.Close()
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}

func copyDirectory(src, dst string) error {
	cleanSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	cleanDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	return filepath.Walk(cleanSrc, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cleanSrc, path)
		if err != nil {
			return err
		}
		target := filepath.Join(cleanDst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		_, err = io.Copy(out, in)
		return err
	})
}

func (r *Runner) extractCustomRules(dir string, report *VerificationReport) {
	canonicalDir, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	if evaled, err := filepath.EvalSymlinks(canonicalDir); err == nil {
		canonicalDir = evaled
	}

	root, err := os.OpenRoot(canonicalDir)
	if err != nil {
		return
	}
	defer root.Close()

	readBoundedRule := func(relPath string) (string, bool) {
		cleanRel := filepath.Clean(relPath)
		if err := ValidateSnapshotPath(cleanRel); err != nil {
			return "", false
		}
		f, err := root.Open(filepath.FromSlash(cleanRel))
		if err != nil {
			return "", false
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
			return "", false
		}

		if err := verifySnapshotFileHandle(f, canonicalDir); err != nil {
			return "", false
		}

		// Stream up to 15000 bytes + 1 to detect truncation without unbounded allocation
		const maxRuleBytes = 15000
		limited := io.LimitReader(f, maxRuleBytes+1)
		data, err := io.ReadAll(limited)
		if err != nil || len(bytes.TrimSpace(data)) == 0 {
			return "", false
		}

		if len(data) > maxRuleBytes {
			return string(data[:maxRuleBytes]) + "\n...[instructions truncated]...", true
		}
		return string(data), true
	}

	priorityList := []string{
		".github/copilot-instructions.md",
		".github/instructions.md",
		".github/CONTRIBUTING.md",
		"AGENTS.md",
		"CLAUDE.md",
		"CONTRIBUTING.md",
		".cursorrules",
	}

	for _, rel := range priorityList {
		if content, ok := readBoundedRule(rel); ok {
			report.CustomRules = content
			report.RulesSource = rel
			return
		}
	}

	keywords := []string{"instruct", "guide", "rule", "contribut", "agent", "standard", "convention"}
	scanDirs := []string{".", ".github"}

	for _, sDir := range scanDirs {
		var entries []os.DirEntry
		if sDir == "." {
			entries, err = os.ReadDir(canonicalDir)
		} else {
			entries, err = os.ReadDir(filepath.Join(canonicalDir, sDir))
		}
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			lowerName := strings.ToLower(entry.Name())
			if !strings.HasSuffix(lowerName, ".md") && !strings.HasSuffix(lowerName, "rules") {
				continue
			}

			for _, kw := range keywords {
				if strings.Contains(lowerName, kw) {
					rel := entry.Name()
					if sDir != "." {
						rel = sDir + "/" + entry.Name()
					}
					if content, ok := readBoundedRule(rel); ok {
						report.CustomRules = content
						report.RulesSource = rel
						return
					}
				}
			}
		}
	}
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen] + "\n...[instructions truncated]..."
	}
	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
