package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

// TestWriteSandboxEnvFile covers atomic creation/update of the operator
// environment file, including 0600 permissions and preservation of unrelated
// configuration.
func TestWriteSandboxEnvFile(t *testing.T) {
	t.Run("creates new file with required variables", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pr-review.env")
		err := writeSandboxEnvFile(path, "sha256:abc123", "/var/lib/pr-review/slots/slot-01", "/var/lib/pr-review/control")
		if err != nil {
			t.Fatalf("writeSandboxEnvFile: %v", err)
		}

		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("permissions = %o, want 0600", got)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, "# pr-review-go sandbox configuration") {
			t.Errorf("missing header comment:\n%s", content)
		}
		for _, want := range []string{
			"ENABLE_SANDBOX=true",
			"SANDBOX_MODE=podman",
			"SANDBOX_IMAGE=sha256:abc123",
			"SANDBOX_SLOT_DIR=/var/lib/pr-review/slots/slot-01",
			"SANDBOX_CONTROL_DIR=/var/lib/pr-review/control",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("missing %q in:\n%s", want, content)
			}
		}
	})

	t.Run("updates existing file preserving unrelated lines", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pr-review.env")
		original := "# operator config\nGITHUB_TOKEN=secret\nPORT=3000\n\nSANDBOX_IMAGE=old-image\nWEBHOOK_SECRET=hook\n"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatalf("seed env file: %v", err)
		}

		err := writeSandboxEnvFile(path, "sha256:newdigest", "/srv/slot", "/srv/control")
		if err != nil {
			t.Fatalf("writeSandboxEnvFile: %v", err)
		}

		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("permissions after update = %o, want 0600", got)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		content := string(data)
		lines := strings.Split(content, "\n")

		for _, preserved := range []string{
			"# operator config",
			"GITHUB_TOKEN=secret",
			"PORT=3000",
			"WEBHOOK_SECRET=hook",
			"",
		} {
			found := false
			for _, line := range lines {
				if line == preserved {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("preserved line %q missing from:\n%s", preserved, content)
			}
		}

		for _, want := range []string{
			"ENABLE_SANDBOX=true",
			"SANDBOX_MODE=podman",
			"SANDBOX_IMAGE=sha256:newdigest",
			"SANDBOX_SLOT_DIR=/srv/slot",
			"SANDBOX_CONTROL_DIR=/srv/control",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("missing %q in:\n%s", want, content)
			}
		}
		if strings.Contains(content, "SANDBOX_IMAGE=old-image") {
			t.Errorf("stale SANDBOX_IMAGE retained:\n%s", content)
		}
		if strings.Count(content, "SANDBOX_IMAGE=") != 1 {
			t.Errorf("expected exactly one SANDBOX_IMAGE line:\n%s", content)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		if err := writeSandboxEnvFile("", "img", "/slot", "/control"); err == nil {
			t.Fatal("expected error for empty path")
		}
	})
}

// TestIsMountedFromReader covers /proc/self/mountinfo parsing, including
// malformed lines, clean/trailing-slash normalization, and unknown mounts.
func TestIsMountedFromReader(t *testing.T) {
	const mountPoint = "/var/lib/pr-review/slots/slot-01"
	const sampleMountInfo = `22 1 253:0 / / rw,relatime shared:1 - ext4 /dev/mapper/root rw
36 22 7:0 / /var/lib/pr-review/slots/slot-01 rw,nodev,nosuid - ext4 /dev/loop0 rw
40 22 0:5 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
`

	tests := []struct {
		name    string
		content string
		dir     string
		want    bool
	}{
		{
			name:    "matching mount point",
			content: sampleMountInfo,
			dir:     mountPoint,
			want:    true,
		},
		{
			name:    "non-matching mount point",
			content: sampleMountInfo,
			dir:     "/var/lib/pr-review/slots/slot-02",
			want:    false,
		},
		{
			name:    "lines with too few fields are skipped",
			content: "1 2 3\n4 5\n" + sampleMountInfo,
			dir:     mountPoint,
			want:    true,
		},
		{
			name:    "trailing slash is normalized",
			content: sampleMountInfo,
			dir:     mountPoint + "/",
			want:    true,
		},
		{
			name:    "empty reader",
			content: "",
			dir:     mountPoint,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isMountedFromReader(strings.NewReader(tt.content), tt.dir)
			if err != nil {
				t.Fatalf("isMountedFromReader: %v", err)
			}
			if got != tt.want {
				t.Errorf("isMountedFromReader(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}

// TestFirstNonEmpty covers sandbox setup default resolution.
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
	}
	if got := firstNonEmpty("", "", ""); got != "" {
		t.Errorf("firstNonEmpty(all empty) = %q, want empty", got)
	}
	if got := firstNonEmpty("", "second", "third"); got != "second" {
		t.Errorf("firstNonEmpty = %q, want second", got)
	}
}

// TestSetupSandbox_NonLinuxAndMissingPodman verifies fail-closed platform and
// dependency handling without touching host state.
func TestSetupSandbox_NonLinuxAndMissingPodman(t *testing.T) {
	t.Run("non-linux refusal", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := sandbox.RunSetupWithDeps(nil, sandbox.SetupOptions{}, &out, &errOut, sandbox.SetupDeps{GOOS: "darwin"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "only supported on Linux") {
			t.Errorf("stderr missing Linux-only message: %s", errOut.String())
		}
	})

	t.Run("missing podman", func(t *testing.T) {
		var out, errOut bytes.Buffer
		deps := sandbox.SetupDeps{
			GOOS:     "linux",
			LookPath: func(string) (string, error) { return "", errors.New("executable file not found in $PATH") },
		}
		code := sandbox.RunSetupWithDeps(nil, sandbox.SetupOptions{}, &out, &errOut, deps)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "podman not found in PATH") {
			t.Errorf("stderr missing podman install guidance: %s", errOut.String())
		}
	})

	t.Run("podman version failure", func(t *testing.T) {
		var out, errOut bytes.Buffer
		deps := sandbox.SetupDeps{
			GOOS:           "linux",
			LookPath:       func(string) (string, error) { return "/usr/bin/podman", nil },
			CombinedOutput: func(string, ...string) ([]byte, error) { return []byte("broken"), errors.New("exit status 125") },
		}
		code := sandbox.RunSetupWithDeps(nil, sandbox.SetupOptions{}, &out, &errOut, deps)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "podman --version failed") {
			t.Errorf("stderr missing version failure: %s", errOut.String())
		}
	})
}

// TestContainerfile_CopyDirectories guards the sandbox build context: the
// trusted helper build needs both full Go trees.
func TestContainerfile_CopyDirectories(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "sandbox", "Containerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(data)
	for _, want := range []string{"COPY pkg pkg", "COPY cmd cmd"} {
		if !strings.Contains(content, want) {
			t.Errorf("Containerfile missing %q:\n%s", want, content)
		}
	}
}
