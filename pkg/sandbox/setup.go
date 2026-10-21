package sandbox

import (
	"bufio"
	"fmt"
	"io"
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
// GHCR tag; after a pull the resolved content digest is reported so operators
// can pin SANDBOX_IMAGE to an immutable sha256 reference.
const (
	DefaultSandboxSetupImage    = "ghcr.io/thozoz/pr-review-go-sandbox:latest"
	DefaultSandboxSetupSlotDir  = "/var/lib/pr-review/slots/slot-01"
	DefaultSandboxSetupSlotSize = "3G"
	DefaultSandboxControlDir    = "/tmp/pr-review-sandbox-control"
)

// SetupOptions are the operator-selected inputs for sandbox provisioning.
type SetupOptions struct {
	// Image is the sandbox image to pull or build. Empty falls back to
	// cfg.SandboxImage and then DefaultSandboxSetupImage.
	Image string
	// SlotDir is the mount point for the dedicated ext4 slot. Empty falls
	// back to cfg.SandboxSlotDir and then DefaultSandboxSetupSlotDir.
	SlotDir string
	// SlotSize is the backing-file size for a newly created slot.
	SlotSize string
	// BuildFromSource builds deploy/sandbox/Containerfile locally instead of
	// pulling the image. Builds use --network=host for unprivileged LXC hosts.
	BuildFromSource bool
	// WriteEnvPath, when non-empty, persists the resolved sandbox
	// configuration to an operator environment file with 0600 permissions.
	WriteEnvPath string
}

// SetupDeps are the host interactions used by RunSetup. Zero-value fields fall
// back to the real implementations; tests inject fakes for platform and PATH
// behavior without touching host state.
type SetupDeps struct {
	// GOOS is the target operating system reports. Empty means runtime.GOOS.
	GOOS string
	// LookPath resolves an executable. Empty means exec.LookPath.
	LookPath func(file string) (string, error)
	// CombinedOutput runs a command and returns combined stdout/stderr.
	// Empty means exec.Command(...).CombinedOutput.
	CombinedOutput func(name string, args ...string) ([]byte, error)
	// IsMounted reports whether dir is a mount point. Empty means IsMounted.
	// It carries no effect on the zero-value path unless defaulted here.
	// Kept for tests that need to bypass /proc/self/mountinfo.
	IsMounted func(dir string) (bool, error)
	// ProbeFSType probes a backing file's filesystem type. Empty means
	// blkidFSType.
	ProbeFSType func(path string) (string, error)
	// ChownSlot hands the slot to the service user. Empty means
	// chownSlotToServiceUser.
	ChownSlot func(slotDir string) error
}

func defaultSetupDeps() SetupDeps {
	return SetupDeps{
		GOOS:     runtime.GOOS,
		LookPath: exec.LookPath,
		CombinedOutput: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
		IsMounted:   IsMounted,
		ProbeFSType: blkidFSType,
		ChownSlot:   chownSlotToServiceUser,
	}
}

// withDefaults fills every unset dependency.
func (d SetupDeps) withDefaults() SetupDeps {
	def := defaultSetupDeps()
	if d.GOOS == "" {
		d.GOOS = def.GOOS
	}
	if d.LookPath == nil {
		d.LookPath = def.LookPath
	}
	if d.CombinedOutput == nil {
		d.CombinedOutput = def.CombinedOutput
	}
	if d.IsMounted == nil {
		d.IsMounted = def.IsMounted
	}
	if d.ProbeFSType == nil {
		d.ProbeFSType = def.ProbeFSType
	}
	if d.ChownSlot == nil {
		d.ChownSlot = def.ChownSlot
	}
	return d
}

// RunSetup provisions sandbox prerequisites on Linux and returns a process
// exit code: podman presence, port availability, the loop ext4 slot (with
// post-mount validation), and the trusted sandbox image. It never formats
// existing block devices — only the dedicated slot backing file
// (<slotDir>.img).
func RunSetup(cfg *config.Config, opts SetupOptions, out, errOut io.Writer) int {
	return RunSetupWithDeps(cfg, opts, out, errOut, SetupDeps{})
}

// RunSetupWithDeps is RunSetup with injectable host interactions for tests.
func RunSetupWithDeps(cfg *config.Config, opts SetupOptions, out, errOut io.Writer, deps SetupDeps) int {
	deps = deps.withDefaults()
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}

	if deps.GOOS != "linux" {
		fmt.Fprintln(errOut, "ERROR: --setup-sandbox is only supported on Linux (fixed ext4 slot leasing is Linux-only).")
		return 1
	}

	image := opts.Image
	if image == "" && cfg != nil {
		image = cfg.SandboxImage
	}
	if image == "" {
		image = DefaultSandboxSetupImage
	}
	slotDir := opts.SlotDir
	if slotDir == "" && cfg != nil {
		slotDir = cfg.SandboxSlotDir
	}
	if slotDir == "" {
		slotDir = DefaultSandboxSetupSlotDir
	}
	slotSize := opts.SlotSize
	if slotSize == "" {
		slotSize = DefaultSandboxSetupSlotSize
	}
	controlDir := DefaultSandboxControlDir
	if cfg != nil && cfg.SandboxControlDir != "" {
		controlDir = cfg.SandboxControlDir
	}

	fail := func(format string, args ...any) int {
		fmt.Fprintf(errOut, "ERROR: "+format+"\n", args...)
		return 1
	}

	fmt.Fprintln(out, "── pr-review-go sandbox setup ──")

	// 1. Podman presence.
	podmanBin, err := deps.LookPath("podman")
	if err != nil {
		return fail("podman not found in PATH. Install Podman 5.8+ for the service user, then re-run.")
	}
	versionOut, err := deps.CombinedOutput(podmanBin, "--version")
	if err != nil {
		return fail("podman --version failed: %v (%s)", err, strings.TrimSpace(string(versionOut)))
	}
	fmt.Fprintf(out, "[1/5] podman: %s (%s)\n", strings.TrimSpace(string(versionOut)), podmanBin)
	if infoOut, err := deps.CombinedOutput(podmanBin, "info", "--format", "json"); err != nil {
		fmt.Fprintf(out, "      WARN: podman info failed — rootless/cgroup-v2 attestation unavailable: %v\n", err)
		fmt.Fprintln(out, "      Hint: run 'podman info' as the service user; rootful podman is denied by the backend")
		fmt.Fprintln(out, "      unless ALLOW_ROOTFUL_SANDBOX=1 is set for a pre-isolated environment (LXC/CI).")
		_ = infoOut
	} else {
		fmt.Fprintln(out, "      podman info: OK")
	}

	// 2. Rootless network / TUN pre-check (Proxmox unprivileged LXC lesson).
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		fmt.Fprintln(out, "[2/5] WARN: /dev/net/tun missing — podman's default pasta network cannot allocate TAP devices here.")
		fmt.Fprintln(out, "      Pulling a prebuilt image avoids this entirely. For local builds use --network=host,")
		fmt.Fprintln(out, "      or pass the host TUN device into the container (Proxmox: pct set <ID> -mp0 /dev/net/tun,mp=/dev/net/tun).")
	} else {
		fmt.Fprintln(out, "[2/5] /dev/net/tun present.")
	}

	// 3. Port availability (stale daemon lesson).
	if ln, err := net.Listen("tcp", "127.0.0.1:3000"); err != nil {
		fmt.Fprintf(out, "[3/5] WARN: port 3000 unavailable: %v\n", err)
		fmt.Fprintln(out, "      Hint: find the holder with 'ss -tlnp | grep :3000' and stop the stale daemon before starting the server.")
	} else {
		_ = ln.Close()
		fmt.Fprintln(out, "[3/5] port 3000: free.")
	}

	// 4. 3 GiB loop ext4 slot.
	imgPath := slotDir + ".img"
	label := "pr-slot-01"
	if base := filepath.Base(slotDir); base != "" {
		label = "pr-" + strings.ReplaceAll(base, "_", "-")
	}
	fmt.Fprintf(out, "[4/5] slot: %s (backing %s, size %s)\n", slotDir, imgPath, slotSize)
	if _, err := os.Stat(imgPath); os.IsNotExist(err) {
		fmt.Fprintf(out, "      creating sparse backing file (%s)...\n", slotSize)
		if truncOut, err := deps.CombinedOutput("truncate", "-s", slotSize, imgPath); err != nil {
			return fail("truncate failed: %v (%s). As root: truncate -s %s %s", err, strings.TrimSpace(string(truncOut)), slotSize, imgPath)
		}
	}
	fstype, err := deps.ProbeFSType(imgPath)
	if err != nil {
		fmt.Fprintf(out, "      WARN: could not probe filesystem type: %v\n", err)
	}
	if fstype != "ext4" {
		fmt.Fprintf(out, "      formatting ext4 (label %s)...\n", label)
		if mkfsOut, err := deps.CombinedOutput("mkfs.ext4", "-F", "-L", label, imgPath); err != nil {
			return fail("mkfs.ext4 failed: %v (%s). Refusing to continue without a dedicated ext4 backing file.", err, strings.TrimSpace(string(mkfsOut)))
		}
	} else {
		fmt.Fprintln(out, "      backing file already ext4.")
	}
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return fail("mkdir %s failed: %v", slotDir, err)
	}
	mounted, err := deps.IsMounted(slotDir)
	if err != nil {
		fmt.Fprintf(out, "      WARN: mount table check failed: %v\n", err)
	}
	if !mounted {
		fmt.Fprintln(out, "      mounting (loop,nodev,nosuid)...")
		if mountOut, err := deps.CombinedOutput("mount", "-o", "loop,nodev,nosuid", imgPath, slotDir); err != nil {
			fmt.Fprintf(out, "      WARN: mount failed: %v (%s)\n", err, strings.TrimSpace(string(mountOut)))
			fmt.Fprintln(out, "      Proxmox unprivileged LXC cannot mount loop devices without host passthrough.")
			fmt.Fprintf(out, "      Host alternative: pct set <ID> -mp0 %s,mp=%s\n", imgPath, slotDir)
			fmt.Fprintln(out, "      Or add to /etc/fstab and run 'mount -a':")
			fmt.Fprintf(out, "        %s %s ext4 loop,nodev,nosuid 0 0\n", imgPath, slotDir)
		} else {
			fmt.Fprintln(out, "      mounted.")
		}
	} else {
		fmt.Fprintln(out, "      already mounted.")
	}
	if err := deps.ChownSlot(slotDir); err != nil {
		fmt.Fprintf(out, "      WARN: chown skipped: %v\n", err)
	}
	fmt.Fprintln(out, "      persistence (append to /etc/fstab):")
	fmt.Fprintf(out, "        %s %s ext4 loop,nodev,nosuid 0 0\n", imgPath, slotDir)

	// 4b. Post-mount health verification with the same manager the server uses.
	slotMgr, err := NewLinuxSlotManager(slotDir, controlDir)
	if err != nil {
		return fail("slot manager initialization failed: %v", err)
	}
	if slotMgr.SkipMountChecks {
		fmt.Fprintln(out, "[4b/5] slot validation: SKIPPED (SANDBOX_SKIP_MOUNT_CHECKS active).")
	} else if mountID, err := slotMgr.ValidateSlotMount(slotDir); err != nil {
		fmt.Fprintf(out, "[4b/5] slot validation: WARN: %v\n", err)
		fmt.Fprintln(out, "      Verify the slot parent directory is owned by root, the backing file is ext4,")
		fmt.Fprintln(out, "      the mount enforces nodev,nosuid, and total capacity is <= 3 GiB.")
		fmt.Fprintln(out, "      On unprivileged LXC hosts without loop passthrough, verify the mount on the host")
		fmt.Fprintln(out, "      first, then opt out explicitly with SANDBOX_SKIP_MOUNT_CHECKS=1.")
	} else {
		fmt.Fprintf(out, "[4b/5] slot validation: OK (mount ID: %d, ext4, nodev, nosuid, capacity <= 3 GiB).\n", mountID)
	}

	// 5. Trusted image.
	fmt.Fprintf(out, "[5/5] image: %s\n", image)
	if opts.BuildFromSource {
		fmt.Fprintln(out, "      building from deploy/sandbox/Containerfile with --network=host (LXC pasta workaround)...")
		buildOut, err := deps.CombinedOutput(podmanBin, "build", "--network=host", "-f", "deploy/sandbox/Containerfile", "-t", image, ".")
		fmt.Fprint(out, string(buildOut))
		if err != nil {
			return fail("podman build failed: %v", err)
		}
	} else {
		fmt.Fprintln(out, "      pulling (prebuilt image avoids in-LXC builds entirely)...")
		pullOut, err := deps.CombinedOutput(podmanBin, "pull", image)
		fmt.Fprint(out, string(pullOut))
		if err != nil {
			return fail("podman pull failed: %v. Retry with --build-from-source to build locally.", err)
		}
	}
	resolvedImage := image
	if digestOut, err := deps.CombinedOutput(podmanBin, "image", "inspect", "--format", "{{.Digest}}", image); err == nil {
		if digest := strings.TrimSpace(string(digestOut)); digest != "" && digest != "<nil>" {
			fmt.Fprintf(out, "      image digest: %s\n", digest)
			fmt.Fprintf(out, "      pin with: SANDBOX_IMAGE=%s\n", digest)
			resolvedImage = digest
		}
	}

	fmt.Fprintln(out, "── setup complete ──")
	fmt.Fprintln(out, "Export and restart the server with:")
	fmt.Fprintf(out, "  ENABLE_SANDBOX=true\n  SANDBOX_MODE=podman\n  SANDBOX_IMAGE=%s   # prefer the pinned digest above\n  SANDBOX_SLOT_DIR=%s\n  SANDBOX_CONTROL_DIR=%s\n",
		resolvedImage, slotDir, controlDir)
	fmt.Fprintln(out, "Without these, the server stays fail-closed (StatusUnavailable + honest notice, no host execution).")
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

// IsMounted reports whether dir is a mount point per /proc/self/mountinfo.
func IsMounted(dir string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	return IsMountedFromReader(f, dir)
}

// IsMountedFromReader reports whether dir appears in mountinfo data read from r.
func IsMountedFromReader(r io.Reader, dir string) (bool, error) {
	clean := filepath.Clean(dir)
	sc := bufio.NewScanner(r)
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

// FirstNonEmpty returns the first non-empty string, or "" when all are empty.
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
