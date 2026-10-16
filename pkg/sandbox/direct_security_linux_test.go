//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDirectPrivateRoot(t *testing.T) {
	if err := AttestBwrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := writeDirectModule(t, map[string]string{"go.mod": "module example.com/probe\n\ngo 1.24\n", "main.go": "package main\nfunc main() {}\n"})
	paths := provisionTestJobPaths(t)
	secret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(secret, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UNKNOWN_SERVICE_SECRET", "must-not-leak")
	// Positive control verifies the sentinel exists outside the private root.
	if _, err := os.ReadFile(secret); err != nil {
		t.Fatal(err)
	}
	// When the Go toolchain lives under the host home (a module-managed
	// toolchain under ~/go/pkg/mod), bwrap necessarily materialises the empty
	// /home parent chain for that read-only bind, so an unconditional
	// `test ! -e /home` is wrong on such hosts. Prove the host home's private
	// state is still invisible instead.
	homeProbe := "test ! -e /home"
	if goroot, err := filepath.EvalSymlinks(runtime.GOROOT()); err == nil && goroot != "" {
		if hostHome, err := os.UserHomeDir(); err == nil && hostHome != "" &&
			strings.HasPrefix(goroot, filepath.Clean(hostHome)+string(os.PathSeparator)) {
			homeProbe = fmt.Sprintf("test ! -e %q && test ! -e %q",
				filepath.Join(hostHome, ".ssh"), filepath.Join(hostHome, ".config"))
		}
	}
	script := "set -eu; test ! -e '" + secret + "'; test -z \"${UNKNOWN_SERVICE_SECRET:-}\"; test ! -e /run/sandbox/gateway.sock; " + homeProbe + "; test ! -e /root; touch /work/allowed; if touch /usr/bin/forbidden 2>/dev/null; then exit 1; fi; go build ./..."
	argv, err := directIsolatedArgv(work, paths.CacheDir, paths.TmpDir, []string{"/bin/sh", "-c", script}, "", "")
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
	paths := provisionTestJobPaths(t)
	res, err := directPrepare(context.Background(), time.Minute, work, paths.CacheDir, paths.TmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatal(res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(paths.GoModCache, "rsc.io/quote@v1.5.2")); err != nil {
		t.Fatal("gateway did not fill private cache: ", err)
	}
}

func TestDirectIsolationMountContract(t *testing.T) {
	paths := provisionTestJobPaths(t)
	argv, err := directIsolatedArgv(t.TempDir(), paths.CacheDir, paths.TmpDir, []string{"go", "test", "-race", "./..."}, "", "")
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
	if _, err := directIsolatedArgv("/", paths.CacheDir, paths.TmpDir, []string{"go", "test", "./..."}, "", ""); err == nil {
		t.Fatal("root workspace accepted")
	}
}
