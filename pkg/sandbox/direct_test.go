package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const directTestSHA = "0123456789abcdef0123456789abcdef01234567"

func writeDirectModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
}

func TestDirectRunner_Passing(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":      "module example.com/directpass\n\ngo 1.24\n",
		"main.go":     "package main\n\nfunc Add(a, b int) int { return a + b }\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed, got %q (%s)\nstdout: %s\nstderr: %s",
			report.Status, report.Reason, report.Results[0].Stdout, lastResultStderr(report))
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 stage results, got %d", len(report.Results))
	}
	for _, res := range report.Results {
		if !res.Passed {
			t.Errorf("expected stage %q passed", res.Command)
		}
	}
}

func TestDirectRunner_BuildFailure(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/directbuildfail\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc broken( {\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusBuildFailed {
		t.Fatalf("expected StatusBuildFailed, got %q (%s)", report.Status, report.Reason)
	}
	if report.FailedStage != "build" {
		t.Errorf("expected FailedStage build, got %q", report.FailedStage)
	}
	if len(report.Results) != 1 {
		t.Errorf("expected 1 stage result (build only), got %d", len(report.Results))
	}
}

func TestDirectRunner_TestFailure(t *testing.T) {
	requireGoToolchain(t)
	dir := writeDirectModule(t, map[string]string{
		"go.mod":      "module example.com/directtestfail\n\ngo 1.24\n",
		"main.go":     "package main\n\nfunc Add(a, b int) int { return a + b }\n\nfunc main() {}\n",
		"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 99 {\n\t\tt.Fatal(\"intentional failure\")\n\t}\n}\n",
	})

	d := NewDirectRunner(2 * time.Minute)
	report, err := d.VerifyDir(context.Background(), dir, directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusTestFailed {
		t.Fatalf("expected StatusTestFailed, got %q (%s)", report.Status, report.Reason)
	}
	if report.FailedStage != "test" {
		t.Errorf("expected FailedStage test, got %q", report.FailedStage)
	}
	if len(report.Results) != 2 {
		t.Errorf("expected 2 stage results, got %d", len(report.Results))
	}
}

func TestDirectRunner_UnknownDirPassesWithNotice(t *testing.T) {
	d := NewDirectRunner(time.Minute)
	report, err := d.VerifyDir(context.Background(), t.TempDir(), directTestSHA)
	if err != nil {
		t.Fatalf("VerifyDir error: %v", err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("expected StatusPassed for unknown project, got %q", report.Status)
	}
	if len(report.Results) != 0 {
		t.Errorf("expected 0 stage results for unknown project, got %d", len(report.Results))
	}
}

func TestDetectDirectProject(t *testing.T) {
	cases := []struct {
		name     string
		marker   string
		lang     string
		tool     string
		hasBuild bool
	}{
		{"go", "go.mod", "go", "go", true},
		{"node", "package.json", "node", "npm", false},
		{"rust", "Cargo.toml", "rust", "cargo", false},
		{"python-pyproject", "pyproject.toml", "python", "pytest", false},
		{"python-requirements", "requirements.txt", "python", "pytest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeDirectModule(t, map[string]string{tc.marker: ""})
			proj := detectDirectProject(dir)
			if proj == nil {
				t.Fatalf("expected project detected for %s", tc.marker)
			}
			if proj.lang != tc.lang || proj.tool != tc.tool {
				t.Errorf("got lang=%q tool=%q, want %q %q", proj.lang, proj.tool, tc.lang, tc.tool)
			}
			if (len(proj.buildArgs) > 0) != tc.hasBuild {
				t.Errorf("build step presence mismatch for %s", tc.lang)
			}
			if len(proj.testArgs) == 0 {
				t.Errorf("expected test args for %s", tc.lang)
			}
		})
	}
	t.Run("unknown", func(t *testing.T) {
		if proj := detectDirectProject(t.TempDir()); proj != nil {
			t.Errorf("expected nil for empty dir, got %+v", proj)
		}
	})
}

func lastResultStderr(report *VerificationReport) string {
	if len(report.Results) == 0 {
		return ""
	}
	return report.Results[len(report.Results)-1].Stderr
}
