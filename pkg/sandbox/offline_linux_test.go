//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Manual-only companion proofs (operator steps per 07-VALIDATION.md
// "Manual-Only Verifications", not automatable from the Windows dev box):
//   - Podman --network=none stage-2 enforcement on a Linux host: run the
//     stage-2 container, attempt dial-out from a test binary, confirm no
//     route; confirm no gateway socket mount.
//   - bwrap availability + userns+netns probe on the Proxmox CT:
//     `command -v bwrap && bwrap --version`, `unshare -Urn true`; confirm the
//     REDUCED-GUARANTEE label path when unavailable.
//
// TestOfflineDialOutLinux is the automated half: it proves enforcement beyond
// env assertion for the direct backend wrap on a bwrap-capable Linux host. It
// skips cleanly where bwrap/userns is absent (D-11 degraded path) — never
// fails off-target.

// dialProbeSource is a tiny Go program reporting network reachability: exit 0
// iff BOTH dials fail (TCP to the host-loopback listener and unix to the
// gateway socket path), exit 1 otherwise.
const dialProbeSource = `package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	addr := os.Args[1]
	sock := os.Args[2]
	tcpOK := false
	if c, err := net.DialTimeout("tcp", addr, 3*time.Second); err == nil {
		tcpOK = true
		_ = c.Close()
	} else {
		fmt.Fprintf(os.Stderr, "tcp dial failed (isolated): %v\n", err)
	}
	unixOK := false
	if c, err := net.DialTimeout("unix", sock, 3*time.Second); err == nil {
		unixOK = true
		_ = c.Close()
	} else {
		fmt.Fprintf(os.Stderr, "unix dial failed (isolated): %v\n", err)
	}
	if tcpOK || unixOK {
		fmt.Fprintf(os.Stderr, "REACHABLE: tcp=%v unix=%v\n", tcpOK, unixOK)
		os.Exit(1)
	}
	os.Exit(0)
}
`

func TestOfflineDialOutLinux(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap absent: enforcement probe needs the CT target")
	}
	if err := AttestBwrapRO(context.Background()); err != nil {
		t.Skipf("bwrap-ro rank unavailable, D-11 degraded path applies: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain absent: cannot build the dial probe")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	target := ln.Addr().String()
	sockPath := filepath.Join(t.TempDir(), "depgw-probe.sock")

	probeDir := t.TempDir()
	probeSrc := filepath.Join(probeDir, "dialprobe.go")
	if err := os.WriteFile(probeSrc, []byte(dialProbeSource), 0644); err != nil {
		t.Fatal(err)
	}

	// Positive control, unwrapped: the probe MUST observe reachability
	// (exit 1), proving the probe itself can detect connectivity.
	ctlCtx, ctlCancel := context.WithTimeout(context.Background(), time.Minute)
	defer ctlCancel()
	ctlCmd := exec.CommandContext(ctlCtx, "go", "run", probeSrc, target, sockPath)
	ctlCmd.Dir = probeDir
	if err := ctlCmd.Run(); err == nil {
		t.Fatalf("control probe unwrapped must observe reachability (exit nonzero), got exit 0 for %s", target)
	}

	// Enforcement: the identical probe wrapped in the bwrap-ro read-only-host
	// argv MUST observe isolation (exit 0) — dial failure, not env content.
	wrapCtx, wrapCancel := context.WithTimeout(context.Background(), time.Minute)
	defer wrapCancel()
	paths := provisionTestJobPaths(t)
	wrapped, err := BuildBwrapROStageArgv(
		[]string{"go", "run", "dialprobe.go", target, sockPath},
		DirectJobPaths{WorkspaceDir: probeDir, TmpDir: paths.TmpDir, HomeDir: paths.HomeDir, CacheDir: paths.CacheDir},
		WrapOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	wrapCmd := exec.CommandContext(wrapCtx, wrapped[0], wrapped[1:]...)
	wrapCmd.Dir = probeDir
	if out, err := wrapCmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapped probe must observe isolation (exit 0), got error: %v (output: %s)", err, out)
	}
}
