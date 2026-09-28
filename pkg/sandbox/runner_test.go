package sandbox

import (
	"context"
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
