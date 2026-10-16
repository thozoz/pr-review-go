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

// relaxModuleCache makes a Go module cache removable before the test's
// temp-dir cleanup runs. `go mod download` writes module directories 0555 and
// files 0444, and os.RemoveAll cannot unlink a file from a read-only
// directory, so an untouched cache tree makes t.TempDir's cleanup report a
// permission error.
func relaxModuleCache(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				_ = os.Chmod(path, 0o700)
			} else {
				_ = os.Chmod(path, 0o600)
			}
			return nil
		})
	})
}

func TestDirectAuthorizedGatewayFetch(t *testing.T) {
	if err := AttestBwrapRO(context.Background()); err != nil {
		t.Skipf("bwrap-ro rank unavailable, gateway proof needs the real rank: %v", err)
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
	relaxModuleCache(t, paths.CacheDir)
	// directPrepare binds the spec's workspace at its own path; this fixture
	// supplies its own module tree, so point the job workspace at it.
	paths.WorkspaceDir = work
	res, err := directPrepare(context.Background(), time.Minute, paths, bwrapROConfinementSpec())
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

// TestDirectIsolationMountContract pins the read-only-host mount contract of
// the bwrap-ro rank (Phase 9 success criterion 1). TestDirectPrivateRoot was
// deliberately removed: its confidentiality premise (`test ! -e <host
// secret>`) is false by design once the host root is read-only-bound, and its
// write-confinement assertions live on in the bwrap-ro write matrix.
func TestDirectIsolationMountContract(t *testing.T) {
	paths := provisionTestJobPaths(t)
	argv, err := BuildBwrapROStageArgv([]string{"go", "test", "-race", "./..."}, paths, WrapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsTriple(argv, "--ro-bind", "/", "/") {
		t.Fatalf("argv must bind the host root read-only: %q", argv)
	}
	for i, arg := range argv {
		if arg == "--dev-bind" {
			t.Fatalf("--dev-bind must never appear in the direct path: %q", argv)
		}
		// A read-write host-root bind is the one shape this rank forbids; the
		// required read-only root bind (--ro-bind / /) is asserted above.
		if arg == "--bind" && i+1 < len(argv) && argv[i+1] == "/" {
			t.Fatalf("host root must never be read-write bound: %q", argv)
		}
		// A read-only bind whose destination is "/" would remap another tree
		// onto the root; only the host root itself may target "/".
		if arg == "--ro-bind" && i+2 < len(argv) && argv[i+2] == "/" && argv[i+1] != "/" {
			t.Fatalf("only the host root may be bound read-only onto /: %q", argv)
		}
	}
	if strings.Contains(strings.Join(argv, " "), "gateway.sock") {
		t.Fatal("stage2 gateway exposed")
	}

	bad := paths
	bad.WorkspaceDir = "/"
	if _, err := BuildBwrapROStageArgv([]string{"go", "test", "./..."}, bad, WrapOptions{}); err == nil {
		t.Fatal("root workspace accepted")
	}
}
