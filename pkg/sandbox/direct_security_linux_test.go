//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirectPrivateRoot(t *testing.T) {
	if err := AttestBwrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := writeDirectModule(t, map[string]string{"go.mod": "module example.com/probe\n\ngo 1.24\n", "main.go": "package main\nfunc main() {}\n"})
	cache, tmp, cleanup, err := directJobDirs()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	secret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(secret, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNKNOWN_SERVICE_SECRET", "must-not-leak")
	// Positive control verifies the sentinel exists outside the private root.
	if _, err := os.ReadFile(secret); err != nil {
		t.Fatal(err)
	}
	script := "set -eu; test ! -e '" + secret + "'; test -z \"${UNKNOWN_SERVICE_SECRET:-}\"; test ! -e /run/sandbox/gateway.sock; test ! -e /home; test ! -e /root; touch /work/allowed; if touch /usr/bin/forbidden 2>/dev/null; then exit 1; fi; go build ./..."
	argv, err := directIsolatedArgv(work, cache, tmp, []string{"/bin/sh", "-c", script}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	res := runDirectStage(context.Background(), time.Minute, work, "probe", argv, nil)
	if !res.Passed {
		t.Fatal(res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(work, "allowed")); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(secret); err != nil || string(contents) != "sentinel" {
		t.Fatalf("host sentinel changed: %s %v", contents, err)
	}
}

func TestDirectAuthorizedGatewayFetch(t *testing.T) {
	if err := AttestBwrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "pr-review-sandbox-helper")); err != nil {
		t.Skip("run compiled suite beside trusted helper for live gateway proof")
	}
	work := writeDirectModule(t, map[string]string{"go.mod": "module example.com/gatewayproof\n\ngo 1.24\n\nrequire rsc.io/quote v1.5.2\n", "main.go": "package main\nfunc main() {}\n"})
	cache, tmp, cleanup, err := directJobDirs()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	res, err := directPrepare(context.Background(), time.Minute, work, cache, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatal(res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(cache, "go/pkg/mod/rsc.io/quote@v1.5.2")); err != nil {
		t.Fatal("gateway did not fill private cache: ", err)
	}
}

func TestDirectIsolationMountContract(t *testing.T) {
	cache, tmp, cleanup, err := directJobDirs()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	argv, err := directIsolatedArgv(t.TempDir(), cache, tmp, []string{"go", "test", "-race", "./..."}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, arg := range argv {
		if (arg == "--bind" || arg == "--ro-bind" || arg == "--dev-bind") && i+1 < len(argv) && argv[i+1] == "/" {
			t.Fatal("host root exposed")
		}
	}
	if strings.Contains(strings.Join(argv, " "), "gateway.sock") {
		t.Fatal("stage2 gateway exposed")
	}
	if _, err := directIsolatedArgv("/", cache, tmp, []string{"go", "test", "./..."}, "", ""); err == nil {
		t.Fatal("root workspace accepted")
	}
}
