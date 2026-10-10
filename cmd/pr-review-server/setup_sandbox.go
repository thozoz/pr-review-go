package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// Default operator values for sandbox provisioning. The image default is the
// GHCR tag; after a pull the resolved content digest is printed so operators
// can pin SANDBOX_IMAGE to an immutable sha256 reference.
const (
	defaultSandboxSetupImage   = "ghcr.io/thozoz/pr-review-go-sandbox:latest"
	defaultSandboxSetupSlotDir = "/var/lib/pr-review/slots/slot-01"
	defaultSandboxSetupSlotLN  = "3G"
)

// runSetupSandbox provisions sandbox prerequisites on Linux and exits with a
// process exit code: podman presence, port availability, 3 GiB loop ext4 slot,
// and the trusted sandbox image. It never formats existing block devices —
// only the dedicated slot backing file (<slotDir>.img).
func runSetupSandbox(cfg *config.Config, image, slotDir, slotSize string, buildFromSource bool) int {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "ERROR: --setup-sandbox is only supported on Linux (fixed ext4 slot leasing is Linux-only).")
		return 1
	}
	if image == "" {
		image = cfg.SandboxImage
	}
	if image == "" {
		image = defaultSandboxSetupImage
	}
	if slotDir == "" {
		slotDir = cfg.SandboxSlotDir
	}
	if slotDir == "" {
		slotDir = defaultSandboxSetupSlotDir
	}
	if slotSize == "" {
		slotSize = defaultSandboxSetupSlotLN
	}
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
		return 1
	}

	fmt.Println("── pr-review-go sandbox setup ──")

	// 1. Podman presence.
	podmanBin, err := exec.LookPath("podman")
	if err != nil {
		return fail("podman not found in PATH. Install Podman 5.8+ for the service user, then re-run.")
	}
	out, err := exec.Command(podmanBin, "--version").CombinedOutput()
	if err != nil {
		return fail("podman --version failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("[1/5] podman: %s (%s)\n", strings.TrimSpace(string(out)), podmanBin)
	if out, err := exec.Command(podmanBin, "info", "--format", "json").CombinedOutput(); err != nil {
		fmt.Printf("      WARN: podman info failed — rootless/cgroup-v2 attestation unavailable: %v\n", err)
		fmt.Printf("      Hint: run 'podman info' as the service user; rootful podman is denied by the backend\n")
		fmt.Printf("      unless ALLOW_ROOTFUL_SANDBOX=1 is set for a pre-isolated environment (LXC/CI).")
		_ = out
	} else {
		fmt.Println("      podman info: OK")
	}

	// 2. Rootless network / TUN pre-check (Proxmox unprivileged LXC lesson).
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		fmt.Println("[2/5] WARN: /dev/net/tun missing — podman's default pasta network cannot allocate TAP devices here.")
		fmt.Println("      Pulling a prebuilt image avoids this entirely. For local builds use --network=host,")
		fmt.Println("      or pass the host TUN device into the container (Proxmox: pct set <ID> -mp0 /dev/net/tun,mp=/dev/net/tun).")
	} else {
		fmt.Println("[2/5] /dev/net/tun present.")
	}

	// 3. Port availability (stale daemon lesson).
	if ln, err := net.Listen("tcp", "127.0.0.1:3000"); err != nil {
		fmt.Printf("[3/5] WARN: port 3000 unavailable: %v\n", err)
		fmt.Println("      Hint: find the holder with 'ss -tlnp | grep :3000' and stop the stale daemon before starting the server.")
	} else {
		_ = ln.Close()
		fmt.Println("[3/5] port 3000: free.")
	}

	// 4. 3 GiB loop ext4 slot.
	imgPath := slotDir + ".img"
	label := "pr-slot-01"
	if base := filepath.Base(slotDir); base != "" {
		label = "pr-" + strings.ReplaceAll(base, "_", "-")
	}
	fmt.Printf("[4/5] slot: %s (backing %s, size %s)\n", slotDir, imgPath, slotSize)
	if _, err := os.Stat(imgPath); os.IsNotExist(err) {
		fmt.Printf("      creating sparse backing file (%s)...\n", slotSize)
		if out, err := exec.Command("truncate", "-s", slotSize, imgPath).CombinedOutput(); err != nil {
			return fail("truncate failed: %v (%s). As root: truncate -s %s %s", err, strings.TrimSpace(string(out)), slotSize, imgPath)
		}
	}
	fstype, err := blkidFSType(imgPath)
	if err != nil {
		fmt.Printf("      WARN: could not probe filesystem type: %v\n", err)
	}
	if fstype != "ext4" {
		fmt.Printf("      formatting ext4 (label %s)...\n", label)
		if out, err := exec.Command("mkfs.ext4", "-F", "-L", label, imgPath).CombinedOutput(); err != nil {
			return fail("mkfs.ext4 failed: %v (%s). Refusing to continue without a dedicated ext4 backing file.", err, strings.TrimSpace(string(out)))
		}
	} else {
		fmt.Println("      backing file already ext4.")
	}
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return fail("mkdir %s failed: %v", slotDir, err)
	}
	mounted, err := isMounted(slotDir)
	if err != nil {
		fmt.Printf("      WARN: mount table check failed: %v\n", err)
	}
	if !mounted {
		fmt.Println("      mounting (loop,nodev,nosuid)...")
		if out, err := exec.Command("mount", "-o", "loop,nodev,nosuid", imgPath, slotDir).CombinedOutput(); err != nil {
			fmt.Printf("      WARN: mount failed: %v (%s)\n", err, strings.TrimSpace(string(out)))
			fmt.Println("      Proxmox unprivileged LXC cannot mount loop devices without host passthrough.")
			fmt.Printf("      Host alternative: pct set <ID> -mp0 %s,mp=%s\n", imgPath, slotDir)
			fmt.Printf("      Or add to /etc/fstab and run 'mount -a':\n")
			fmt.Printf("        %s %s ext4 loop,nodev,nosuid 0 0\n", imgPath, slotDir)
		} else {
			fmt.Println("      mounted.")
		}
	} else {
		fmt.Println("      already mounted.")
	}
	if err := chownSlotToServiceUser(slotDir); err != nil {
		fmt.Printf("      WARN: chown skipped: %v\n", err)
	}
	fmt.Printf("      persistence (append to /etc/fstab):\n")
	fmt.Printf("        %s %s ext4 loop,nodev,nosuid 0 0\n", imgPath, slotDir)

	// 5. Trusted image.
	fmt.Printf("[5/5] image: %s\n", image)
	if buildFromSource {
		fmt.Println("      building from deploy/sandbox/Containerfile with --network=host (LXC pasta workaround)...")
		out, err := exec.Command(podmanBin, "build", "--network=host", "-f", "deploy/sandbox/Containerfile", "-t", image, ".").CombinedOutput()
		fmt.Print(string(out))
		if err != nil {
			return fail("podman build failed: %v", err)
		}
	} else {
		fmt.Println("      pulling (prebuilt image avoids in-LXC builds entirely)...")
		out, err := exec.Command(podmanBin, "pull", image).CombinedOutput()
		fmt.Print(string(out))
		if err != nil {
			return fail("podman pull failed: %v. Retry with --build-from-source to build locally.", err)
		}
	}
	if out, err := exec.Command(podmanBin, "image", "inspect", "--format", "{{.Digest}}", image).CombinedOutput(); err == nil {
		if digest := strings.TrimSpace(string(out)); digest != "" && digest != "<nil>" {
			fmt.Printf("      image digest: %s\n", digest)
			fmt.Printf("      pin with: SANDBOX_IMAGE=%s\n", digest)
		}
	}

	fmt.Println("── setup complete ──")
	fmt.Println("Export and restart the server with:")
	fmt.Printf("  ENABLE_SANDBOX=true\n  SANDBOX_IMAGE=%s   # prefer the pinned digest above\n  SANDBOX_SLOT_DIR=%s\n  SANDBOX_CONTROL_DIR=%s\n",
		image, slotDir, firstNonEmpty(cfg.SandboxControlDir, "/tmp/pr-review-sandbox-control"))
	fmt.Println("Without these, the server stays fail-closed (StatusUnavailable + honest notice, no host execution).")
	return 0
}

// blkidFSType probes the filesystem type of a file without mounting it.
func blkidFSType(path string) (string, error) {
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// isMounted reports whether dir is a mount point per /proc/self/mountinfo.
func isMounted(dir string) (bool, error) {
	clean := filepath.Clean(dir)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if filepath.Clean(fields[4]) == clean {
			return true, nil
		}
	}
	return false, sc.Err()
}

// chownSlotToServiceUser gives the slot to the invoking (sudo) user so the
// rootless service account can lease it. Best-effort: never fatal.
func chownSlotToServiceUser(slotDir string) error {
	target := os.Getenv("SUDO_USER")
	if target == "" {
		if u, err := user.Current(); err == nil {
			target = u.Username
		}
	}
	if target == "" || target == "root" {
		return fmt.Errorf("no non-root service user detected; ensure the service account owns %s", slotDir)
	}
	if out, err := exec.Command("chown", target+":"+target, slotDir).CombinedOutput(); err != nil {
		return fmt.Errorf("chown %s failed: %v (%s)", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
