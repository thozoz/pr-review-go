package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
