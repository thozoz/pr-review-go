package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidateGoMod_Valid(t *testing.T) {
	dir := t.TempDir()
	goModContent := "module example.com/testmod\n\ngo 1.22\n\nrequire golang.org/x/sync v0.7.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateGoMod(dir); err != nil {
		t.Fatalf("expected clean go.mod to pass, got: %v", err)
	}
}

func TestValidateGoMod_InRootReplacement(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subpkg")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	goModContent := "module example.com/testmod\n\ngo 1.22\n\nreplace example.com/sub => ./subpkg\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateGoMod(dir); err != nil {
		t.Fatalf("expected in-root replacement to pass, got: %v", err)
	}
}

func TestValidateGoMod_EscapingReplacement(t *testing.T) {
	dir := t.TempDir()

	testCases := []struct {
		name    string
		content string
	}{
		{
			name:    "parent traversal",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace example.com/foo => ../foo\n",
		},
		{
			name:    "deep parent traversal",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace example.com/foo => sub/../../outside\n",
		},
		{
			name:    "block parent traversal",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace (\n\texample.com/foo => ../outside\n)\n",
		},
		{
			name:    "tab separated parent traversal",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace\texample.com/foo\t=>\t../outside\n",
		},
		{
			name:    "multi space parent traversal",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace   example.com/foo   =>   ../outside\n",
		},
		{
			name:    "tab separated remote module swap",
			content: "module example.com/testmod\n\ngo 1.22\n\nreplace\texample.com/foo\tv1.0.0\t=>\tevil.com/bar\tv1.0.0\n",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testDir := filepath.Join(dir, tc.name)
			_ = os.MkdirAll(testDir, 0755)
			if err := os.WriteFile(filepath.Join(testDir, "go.mod"), []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}

			err := ValidateGoMod(testDir)
			if err == nil {
				t.Fatalf("expected escaping replacement to fail validation")
			}
			if !errors.Is(err, ErrEscapingReplacement) {
				t.Fatalf("expected ErrEscapingReplacement, got: %v", err)
			}
		})
	}
}

func TestValidateGoMod_TabSeparatedReplace(t *testing.T) {
	dir := t.TempDir()
	content := "module example.com/testmod\n\ngo 1.22\n\nreplace\texample.com/a\t=>\t../escape\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	err := ValidateGoMod(dir)
	if err == nil {
		t.Fatalf("expected tab-separated replacement to fail validation")
	}
	if !errors.Is(err, ErrEscapingReplacement) {
		t.Fatalf("expected ErrEscapingReplacement, got: %v", err)
	}
}

func TestValidateGoMod_MultiSpaceReplace(t *testing.T) {
	dir := t.TempDir()
	content := "module example.com/testmod\n\ngo 1.22\n\nreplace    example.com/a    =>    ../escape\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	err := ValidateGoMod(dir)
	if err == nil {
		t.Fatalf("expected multi-space replacement to fail validation")
	}
	if !errors.Is(err, ErrEscapingReplacement) {
		t.Fatalf("expected ErrEscapingReplacement, got: %v", err)
	}
}

type mockDependencyPreparer struct {
	result *StageResult
	err    error
}

func (m *mockDependencyPreparer) PrepareDependencies(ctx context.Context, jobID string, lease *SlotLease) (*StageResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func TestRunner_RunSnapshot_DependencyStatuses(t *testing.T) {
	sourceDir := t.TempDir()
	goModContent := "module example.com/mod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	validSHA := "0123456789abcdef0123456789abcdef01234567"
	snap := &Snapshot{
		CommitSHA: validSHA,
		SourceDir: sourceDir,
	}

	// 1. Incomplete dependency (e.g. newer toolchain, private module without credentials, missing checksum)
	t.Run("IncompleteDependencyYieldsStatusIncomplete", func(t *testing.T) {
		mockSlot := newMockSlotManager(t)
		r := &Runner{
			Timeout:     5 * time.Minute,
			SlotManager: mockSlot,
			Backend:     newMockPodmanBackend(t),
			DepPreparer: &mockDependencyPreparer{
				result: &StageResult{
					StageName:    "prep",
					Passed:       false,
					IsIncomplete: true,
					Stderr:       "INCOMPLETE: toolchain mismatch: requires go >= 1.99",
				},
			},
		}

		// Prep failures are exercised through the open fetch gate: the
		// mapping under test belongs to the fetch stage, which only runs
		// for a trusted opt-in (RunSnapshot defaults closed/offline).
		report, err := r.RunSnapshotWithNetwork(context.Background(), snap, true, true)
		if err != nil {
			t.Fatalf("unexpected RunSnapshot error: %v", err)
		}
		if report.Status != StatusIncomplete {
			t.Errorf("expected StatusIncomplete, got: %q", report.Status)
		}
		if report.FailedStage != "prep" {
			t.Errorf("expected FailedStage 'prep', got: %q", report.FailedStage)
		}
	})

	// 2. Timeout in dependency preparation
	t.Run("TimeoutInDependencyYieldsStatusTimeout", func(t *testing.T) {
		mockSlot := newMockSlotManager(t)
		r := &Runner{
			Timeout:     5 * time.Minute,
			SlotManager: mockSlot,
			Backend:     newMockPodmanBackend(t),
			DepPreparer: &mockDependencyPreparer{
				result: &StageResult{
					StageName: "prep",
					Passed:    false,
					IsTimeout: true,
					Stderr:    "context deadline exceeded",
				},
			},
		}

		// Same open-gate routing as subtest 1: prep mapping needs the fetch stage.
		report, err := r.RunSnapshotWithNetwork(context.Background(), snap, true, true)
		if err != nil {
			t.Fatalf("unexpected RunSnapshot error: %v", err)
		}
		if report.Status != StatusTimeout {
			t.Errorf("expected StatusTimeout, got: %q", report.Status)
		}
		if report.FailedStage != "prep" {
			t.Errorf("expected FailedStage 'prep', got: %q", report.FailedStage)
		}
	})

	// 3. Disk full in dependency preparation
	t.Run("DiskFullInDependencyYieldsStatusDiskExhausted", func(t *testing.T) {
		mockSlot := newMockSlotManager(t)
		r := &Runner{
			Timeout:     5 * time.Minute,
			SlotManager: mockSlot,
			Backend:     newMockPodmanBackend(t),
			DepPreparer: &mockDependencyPreparer{
				result: &StageResult{
					StageName:  "prep",
					Passed:     false,
					IsDiskFull: true,
					Stderr:     "write /cache/go/pkg/mod: no space left on device",
				},
			},
		}

		// Same open-gate routing as subtest 1: prep mapping needs the fetch stage.
		report, err := r.RunSnapshotWithNetwork(context.Background(), snap, true, true)
		if err != nil {
			t.Fatalf("unexpected RunSnapshot error: %v", err)
		}
		if report.Status != StatusDiskExhausted {
			t.Errorf("expected StatusDiskExhausted, got: %q", report.Status)
		}
		if report.FailedStage != "prep" {
			t.Errorf("expected FailedStage 'prep', got: %q", report.FailedStage)
		}
	})
}

func TestNetworkedHooksYieldIncomplete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping container runner tests on non-linux host (requires linux container isolation)")
	}
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "src")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}

	validSHA := "0123456789abcdef0123456789abcdef01234567"
	snap := &Snapshot{
		CommitSHA: validSHA,
		SourceDir: sourceDir,
	}

	cases := []struct {
		name   string
		stderr string
	}{
		{
			name:   "SetupPyExecutionNeedingNetwork",
			stderr: "INCOMPLETE: setup.py install requires network access to index.python.org",
		},
		{
			name:   "NetworkedGoGenerateCodegen",
			stderr: "INCOMPLETE: go:generate tool failed: dial tcp 142.250.190.46:443: i/o timeout",
		},
		{
			name:   "MissingCacheDownloadWithoutOptIn",
			stderr: "INCOMPLETE: module download required but network fetch disabled (opt-in missing)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockSlot := newMockSlotManager(t)
			r := &Runner{
				Timeout:     5 * time.Minute,
				SlotManager: mockSlot,
				Backend:     newMockPodmanBackend(t),
				DepPreparer: &mockDependencyPreparer{
					result: &StageResult{
						StageName:    "prep",
						Passed:       false,
						IsIncomplete: true,
						Stderr:       tc.stderr,
					},
				},
			}

			report, err := r.RunSnapshotWithNetwork(context.Background(), snap, true, true)
			if err != nil {
				t.Fatalf("unexpected RunSnapshotWithNetwork error: %v", err)
			}
			if report.Status != StatusIncomplete {
				t.Errorf("expected StatusIncomplete, got: %q", report.Status)
			}
			if report.Status == StatusPassed {
				t.Errorf("never expected StatusPassed for networked hook")
			}
			if report.FailedStage != "prep" {
				t.Errorf("expected FailedStage 'prep', got: %q", report.FailedStage)
			}
			if !strings.Contains(report.Reason, "INCOMPLETE:") {
				t.Errorf("expected Reason to contain INCOMPLETE: prefix, got: %q", report.Reason)
			}
		})
	}
}

func TestPythonNetworkFetch_YieldsIncomplete(t *testing.T) {
	tempDir := t.TempDir()
	sourceDir := filepath.Join(tempDir, "src")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "requirements.txt"), []byte("pytest==8.0.0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	snap := &Snapshot{
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceDir: sourceDir,
	}

	r := &Runner{
		Timeout: 5 * time.Minute,
		Direct:  NewDirectRunner(2 * time.Minute),
	}

	report, err := r.RunSnapshot(context.Background(), snap)
	if err != nil {
		t.Fatalf("unexpected RunSnapshot error: %v", err)
	}
	if report.Status != StatusIncomplete {
		t.Errorf("expected StatusIncomplete, got: %q", report.Status)
	}
	if report.Status == StatusPassed {
		t.Errorf("never expected StatusPassed for Python network fetch")
	}
	const expected = "INCOMPLETE: Python network fetch not supported in stage 1 (Go-first); rerun with vendored wheels or offline --no-index"
	if !strings.Contains(report.Reason, expected) {
		t.Errorf("expected Reason to contain %q, got: %q", expected, report.Reason)
	}
}



