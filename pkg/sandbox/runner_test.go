package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyProject_GoProject_SkipsExecutionWithoutContainerIsolation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create benign go.mod
	goModContent := "module example.com/testmod\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatalf("failed to create go.mod: %v", err)
	}

	// Create a simple go file
	goFileContent := "package testmod\n\nfunc Hello() string { return \"hello\" }\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "hello.go"), []byte(goFileContent), 0644); err != nil {
		t.Fatalf("failed to create hello.go: %v", err)
	}

	// Add custom rules file to verify rule extraction still works
	rulesContent := "# Project Guidelines\nAlways write tests."
	if err := os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte(rulesContent), 0644); err != nil {
		t.Fatalf("failed to create AGENTS.md: %v", err)
	}

	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	report, err := runner.VerifyProject(ctx, tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.DetectedType != "go" {
		t.Errorf("expected DetectedType 'go', got %q", report.DetectedType)
	}

	if report.CustomRules != rulesContent {
		t.Errorf("expected CustomRules %q, got %q", rulesContent, report.CustomRules)
	}
	if report.RulesSource != "AGENTS.md" {
		t.Errorf("expected RulesSource 'AGENTS.md', got %q", report.RulesSource)
	}

	if len(report.Results) != 0 {
		t.Errorf("expected 0 execution results due to missing container isolation, got %d: %+v", len(report.Results), report.Results)
	}

	expectedSubstring := "verification skipped due to missing container isolation"
	if !strings.Contains(report.Summary, "SKIPPED") || !strings.Contains(strings.ToLower(report.Summary), expectedSubstring) {
		t.Errorf("expected summary to explicitly mention SKIPPED and missing container isolation, got: %q", report.Summary)
	}
}

func TestVerifyProject_OtherProjectTypes_SkipExecution(t *testing.T) {
	testCases := []struct {
		name          string
		markerFile    string
		markerContent string
		expectedType  string
	}{
		{
			name:          "Node project",
			markerFile:    "package.json",
			markerContent: `{"name": "test-pkg", "scripts": {"test": "echo test"}}`,
			expectedType:  "node",
		},
		{
			name:          "Rust project",
			markerFile:    "Cargo.toml",
			markerContent: "[package]\nname = \"test-pkg\"\nversion = \"0.1.0\"\n",
			expectedType:  "rust",
		},
		{
			name:          "Python project with pyproject.toml",
			markerFile:    "pyproject.toml",
			markerContent: "[project]\nname = \"test-pkg\"\n",
			expectedType:  "python",
		},
		{
			name:          "Python project with requirements.txt",
			markerFile:    "requirements.txt",
			markerContent: "pytest>=7.0.0\n",
			expectedType:  "python",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmpDir, tc.markerFile), []byte(tc.markerContent), 0644); err != nil {
				t.Fatalf("failed to create %s: %v", tc.markerFile, err)
			}

			runner := NewRunner(10 * time.Second)
			ctx := context.Background()

			report, err := runner.VerifyProject(ctx, tmpDir)
			if err != nil {
				t.Fatalf("VerifyProject failed: %v", err)
			}

			if report.DetectedType != tc.expectedType {
				t.Errorf("expected DetectedType %q, got %q", tc.expectedType, report.DetectedType)
			}

			if len(report.Results) != 0 {
				t.Errorf("expected 0 execution results, got %d: %+v", len(report.Results), report.Results)
			}

			expectedSubstring := "verification skipped due to missing container isolation"
			if !strings.Contains(report.Summary, "SKIPPED") || !strings.Contains(strings.ToLower(report.Summary), expectedSubstring) {
				t.Errorf("expected summary to explicitly mention SKIPPED and missing container isolation, got: %q", report.Summary)
			}
		})
	}
}

func TestVerifyProject_GenericProject(t *testing.T) {
	tmpDir := t.TempDir()
	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	report, err := runner.VerifyProject(ctx, tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.DetectedType != "generic" {
		t.Errorf("expected DetectedType 'generic', got %q", report.DetectedType)
	}
	if len(report.Results) != 0 {
		t.Errorf("expected 0 execution results, got %d", len(report.Results))
	}
	if !strings.Contains(report.Summary, "No recognized build/test configuration found") {
		t.Errorf("unexpected summary: %q", report.Summary)
	}
}

func TestPrepareWorkspace_ReturnsUnavailableWithoutHostGit(t *testing.T) {
	runner := NewRunner(10 * time.Second)
	ctx := context.Background()

	_, _, err := runner.PrepareWorkspace(ctx, "https://github.com/example/repo.git", "main", "0123456789abcdef0123456789abcdef01234567")
	if err == nil {
		t.Fatalf("expected error from PrepareWorkspace, got nil")
	}
	if !errors.Is(err, ErrSourceProviderUnavailable) {
		t.Fatalf("expected ErrSourceProviderUnavailable, got: %v", err)
	}
}

func TestSnapshotValidation_RejectsMalformedAndPrefixSHAs(t *testing.T) {
	tmpDir := t.TempDir()

	testCases := []struct {
		name      string
		commitSHA string
		dir       string
		wantErr   bool
	}{
		{
			name:      "empty SHA",
			commitSHA: "",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "short prefix SHA (7 chars)",
			commitSHA: "abcdef0",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "39 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef0123456",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "40 chars non-hex SHA",
			commitSHA: "0123456789abcdef0123456789abcdef0123456g",
			dir:       tmpDir,
			wantErr:   true,
		},
		{
			name:      "nonexistent directory",
			commitSHA: "0123456789abcdef0123456789abcdef01234567",
			dir:       filepath.Join(tmpDir, "nonexistent"),
			wantErr:   true,
		},
		{
			name:      "valid 40 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef01234567",
			dir:       tmpDir,
			wantErr:   false,
		},
		{
			name:      "valid 64 chars SHA",
			commitSHA: "0123456789abcdef0123456789abcdef012345670123456789abcdef01234567",
			dir:       tmpDir,
			wantErr:   false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Snapshot{
				CommitSHA: tc.commitSHA,
				SourceDir: tc.dir,
			}
			err := s.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSnapshotValidation_RejectsEscapingSymlinks(t *testing.T) {
	outsideDir := t.TempDir()
	sourceDir := t.TempDir()

	targetFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(targetFile, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}

	// Create symlink inside sourceDir pointing outside
	linkPath := filepath.Join(sourceDir, "escape-link")
	if err := os.Symlink(targetFile, linkPath); err != nil {
		t.Fatal(err)
	}

	s := &Snapshot{
		CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		SourceDir: sourceDir,
	}

	err := s.Validate()
	if err == nil {
		t.Fatalf("expected error for escaping symlink, got nil")
	}
	if !strings.Contains(err.Error(), "escaping symlink") {
		t.Fatalf("expected escaping symlink error, got: %v", err)
	}
}

func TestVerifyProject_GoProject_ReportsIncompleteForUnvendoredDependencies(t *testing.T) {
	tmpDir := t.TempDir()

	goModContent := "module example.com/testmod\n\ngo 1.22\n\nrequire github.com/example/external v1.0.0\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644); err != nil {
		t.Fatal(err)
	}

	runner := NewRunnerWithConfig(nil, nil, nil)
	report, err := runner.VerifyProject(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("VerifyProject failed: %v", err)
	}

	if report.Status != StatusIncomplete {
		t.Errorf("expected StatusIncomplete, got %q", report.Status)
	}
	if !strings.Contains(report.Reason, "external dependencies require network gateway") {
		t.Errorf("expected gateway reason, got: %q", report.Reason)
	}
}

