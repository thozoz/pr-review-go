package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// BwrapROHelperPath is the in-sandbox destination of the trusted preparation
// helper for the bwrap-ro rank. Both runtime destinations (this helper and
// BwrapGatewaySocketPath) MUST live inside the private tmpfs mounted at /tmp:
// with `--ro-bind / /` the host root is read-only, so bwrap cannot create a
// mount-point parent under it. Binding the helper or the gateway socket at
// /run/sandbox/... fails with "Can't mkdir parents ... Read-only file system",
// and `--dir /run/sandbox` does not help (T-09-05).
const BwrapROHelperPath = "/tmp/pr-review-runtime/sandbox-helper"

// bwrapROFixedEnvPins are the fixed offline Go pins the rank sets inside the
// wrapper's --clearenv block, independent of the job tree. They are emitted
// before the job-private root pins so a caller-supplied value can never win.
var bwrapROFixedEnvKeys = []string{"GOPROXY", "GOTOOLCHAIN", "GOVCS", "GOWORK", "GOENV", "GO111MODULE"}

// BuildBwrapROStageArgv is the single bwrap-ro argv builder. Every stage of a
// direct run — stage-0 dependency preparation, build, test and in-loop
// commands — derives its argv from this one function through the resolved
// ConfinementSpec (CONFINE-01).
//
// The shape is a read-only *host* root, not a private root remap:
//
//   - `--ro-bind / /` makes the whole host readable but unwritable;
//   - `--dev /dev`, `--proc /proc` and `--tmpfs /tmp` carve out the three
//     trees a build needs to write (`/dev/null`, /proc, and a private tmp);
//   - exactly the job's workspace, tmp, home and cache are bound read-write,
//     each source equal to its destination, so the job-private env pins name
//     paths that are actually writable inside the sandbox;
//   - `--chdir` pins the working directory into the writable workspace so a
//     relative write cannot land on the read-only host root;
//   - `--unshare-net` is retained, so the v1.0 network-removal guarantee is
//     not regressed by this rank.
//
// A refusal is a refusal to execute: callers map a non-nil error to
// StatusUnavailable with zero commands run, never to an unwrapped execution.
func BuildBwrapROStageArgv(command []string, paths DirectJobPaths, opts WrapOptions) ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: direct execution requires Linux mount isolation", ErrBwrapUnavailable)
	}
	if len(command) == 0 {
		return nil, errors.New("bwrap-ro stage argv requires a command")
	}
	for _, r := range []struct {
		name string
		path string
	}{
		{"workspace", paths.WorkspaceDir},
		{"tmp", paths.TmpDir},
		{"home", paths.HomeDir},
		{"cache", paths.CacheDir},
	} {
		if strings.TrimSpace(r.path) == "" {
			return nil, fmt.Errorf("bwrap-ro stage argv requires a non-empty %s path", r.name)
		}
		if filepath.Clean(r.path) == "/" {
			return nil, fmt.Errorf("host root bind refused: %s path %q is the filesystem root", r.name, r.path)
		}
	}
	if opts.GatewaySocketPath != "" && opts.HelperPath == "" {
		return nil, errors.New("trusted preparation helper unavailable")
	}

	goroot, err := filepath.EvalSymlinks(runtime.GOROOT())
	if err != nil {
		return nil, fmt.Errorf("trusted Go toolchain unavailable: %w", err)
	}
	if goroot == "" {
		return nil, errors.New("trusted Go toolchain unavailable: empty GOROOT")
	}

	argv := []string{
		"bwrap",
		"--unshare-user", "--unshare-ipc", "--unshare-pid", "--unshare-net", "--unshare-uts",
		"--die-with-parent", "--new-session",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	for _, p := range []string{paths.WorkspaceDir, paths.TmpDir, paths.HomeDir, paths.CacheDir} {
		argv = append(argv, "--bind", p, p)
	}
	if opts.GatewaySocketPath != "" {
		argv = append(argv,
			"--ro-bind", opts.GatewaySocketPath, BwrapGatewaySocketPath,
			"--ro-bind", opts.HelperPath, BwrapROHelperPath,
		)
	}
	argv = append(argv, "--chdir", paths.WorkspaceDir, "--clearenv")
	argv = append(argv,
		"--setenv", "PATH", goroot+"/bin:/usr/bin:/bin",
		"--setenv", "GOROOT", goroot,
	)
	for _, key := range bwrapROFixedEnvKeys {
		argv = append(argv, "--setenv", key, bwrapROFixedEnvValue(key))
	}
	for _, pin := range directRootEnvPins(paths) {
		argv = append(argv, "--setenv", pin[0], pin[1])
	}
	argv = append(argv, "--")
	return append(argv, command...), nil
}

// bwrapROFixedEnvValue returns the fixed offline value for one key in
// bwrapROFixedEnvKeys. Only keys in that list are accepted.
func bwrapROFixedEnvValue(key string) string {
	switch key {
	case "GOPROXY":
		return "off"
	case "GOTOOLCHAIN":
		return "local"
	case "GOVCS":
		return "off"
	case "GOWORK":
		return "off"
	case "GOENV":
		return "off"
	case "GO111MODULE":
		return "on"
	default:
		return ""
	}
}

// bwrapROWriteControlScript is the enforcement half of the rank probe: the
// read-only root must reject a write under /usr, while the bound scratch
// workspace must accept one. A failure prints an actionable line on stderr and
// exits non-zero.
const bwrapROWriteControlScript = "set -e\nif touch /usr/.pr-review-ro-probe 2>/dev/null; then rm -f /usr/.pr-review-ro-probe; echo 'rank root is writable' >&2; exit 1; fi\ntouch \"$1/write-ok\" && test -f \"$1/write-ok\"\n"

// Probe step names. They are part of the probe's error text so a failing rank
// decision names the exact step that was denied.
const (
	bwrapROStepVersion      = "bwrap --version"
	bwrapROStepMountShape   = "bwrap mount-shape"
	bwrapROStepWriteControl = "bwrap write control"
)

// bwrapROScratch is the throwaway job tree the rank probe binds. It stands in
// for a real job's four writable roots.
type bwrapROScratch struct {
	Root  string
	Work  string
	Tmp   string
	Home  string
	Cache string
}

// bwrapROProbeDeps is the probe seam: every external interaction is injectable
// so the probe's failure modes are unit-testable without bubblewrap.
type bwrapROProbeDeps struct {
	lookPath  func(string) (string, error)
	run       func(ctx context.Context, argv []string) (stderr string, err error)
	mkScratch func() (bwrapROScratch, func(), error)
}

// AttestBwrapRO is the bwrap-ro rank probe. It runs the *production* argv
// shape (scratch binds plus /bin/true) and a write control, so a host where
// `unshare` succeeds but the mount shape is denied reports the rank as
// unavailable instead of running unconfined (T-09-04, PITFALLS #14).
func AttestBwrapRO(ctx context.Context) error {
	return attestBwrapROWith(ctx, defaultBwrapROProbeDeps())
}

// attestBwrapROWith runs the probe against the supplied dependency seam.
func attestBwrapROWith(ctx context.Context, deps bwrapROProbeDeps) error {
	if _, err := deps.lookPath("bwrap"); err != nil {
		return fmt.Errorf("%w: bwrap binary not found on PATH: %v", ErrBwrapUnavailable, err)
	}

	verCtx, verCancel := context.WithTimeout(ctx, 10*time.Second)
	defer verCancel()
	version, err := deps.run(verCtx, []string{"bwrap", "--version"})
	version = strings.TrimSpace(version)
	if err != nil {
		return bwrapROProbeError(bwrapROStepVersion, version, "", err)
	}

	scratch, cleanup, err := deps.mkScratch()
	if err != nil {
		return fmt.Errorf("%w: bwrap probe scratch tree unavailable: %v", ErrBwrapUnavailable, err)
	}
	defer cleanup()
	scratchJob := scratchPaths(scratch)

	// Mount-shape proof: the real argv builder, verbatim.
	shapeCtx, shapeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer shapeCancel()
	shapeArgv, err := BuildBwrapROStageArgv([]string{"/bin/true"}, scratchJob, WrapOptions{})
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBwrapUnavailable, bwrapROStepMountShape, err)
	}
	if stderr, err := deps.run(shapeCtx, shapeArgv); err != nil {
		return bwrapROProbeError(bwrapROStepMountShape, version, stderr, err)
	}

	// Enforcement proof: the root is not writable and the bound scratch tree is.
	ctlCtx, ctlCancel := context.WithTimeout(ctx, 15*time.Second)
	defer ctlCancel()
	ctlArgv, err := BuildBwrapROStageArgv(
		[]string{"/bin/sh", "-c", bwrapROWriteControlScript, "sh", scratch.Work},
		scratchJob, WrapOptions{})
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBwrapUnavailable, bwrapROStepWriteControl, err)
	}
	if stderr, err := deps.run(ctlCtx, ctlArgv); err != nil {
		return bwrapROProbeError(bwrapROStepWriteControl, version, stderr, err)
	}
	return nil
}

// bwrapROProbeError renders an actionable probe failure: the failing step, the
// bwrap version when already known, the captured stderr, and the current
// AppArmor profile. A mount denial under an enforcing Proxmox AppArmor profile
// is otherwise undiagnosable.
func bwrapROProbeError(step, version, stderr string, err error) error {
	if version == "" {
		version = "unknown"
	}
	if strings.TrimSpace(stderr) == "" {
		stderr = "(no stderr captured)"
	}
	return fmt.Errorf("%w: %s failed: %v (bwrap version: %s; stderr: %s; apparmor profile: %s)",
		ErrBwrapUnavailable, step, err, version, strings.TrimSpace(stderr), appArmorProfile())
}

// appArmorProfile reports the process's current AppArmor profile, or
// "unavailable" when /proc/self/attr/current cannot be read.
func appArmorProfile() string {
	data, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return "unavailable"
	}
	profile := strings.TrimSpace(string(data))
	if profile == "" {
		return "unavailable"
	}
	return profile
}

// bwrapProbeBase returns the first writable, already-existing directory among
// /var/tmp, the user cache dir and the shared temporary directory. /var/tmp is
// preferred because the probe's own bind sources are then outside the mount
// the argv shadows with `--tmpfs /tmp`, matching production: config validation
// (validateDirectRootPath) already forbids configured job roots under
// os.TempDir(). Returns "" when no candidate is writable.
func bwrapProbeBase() string {
	candidates := []string{"/var/tmp"}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		candidates = append(candidates, dir)
	}
	candidates = append(candidates, os.TempDir())
	for _, dir := range candidates {
		if bwrapDirWritable(dir) {
			return dir
		}
	}
	return ""
}

// bwrapDirWritable reports whether dir exists as a directory and accepts a
// temporary entry.
func bwrapDirWritable(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	probe, err := os.MkdirTemp(dir, ".pr-review-write-probe-*")
	if err != nil {
		return false
	}
	_ = os.RemoveAll(probe)
	return true
}

// defaultBwrapROProbeDeps wires the probe to the real filesystem and bwrap.
func defaultBwrapROProbeDeps() bwrapROProbeDeps {
	return bwrapROProbeDeps{
		lookPath: exec.LookPath,
		run: func(ctx context.Context, argv []string) (string, error) {
			if len(argv) == 0 {
				return "", errors.New("empty probe argv")
			}
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Run()
			return out.String(), err
		},
		mkScratch: func() (bwrapROScratch, func(), error) {
			base := bwrapProbeBase()
			if base == "" {
				return bwrapROScratch{}, func() {}, fmt.Errorf(
					"no writable probe base directory among /var/tmp, the user cache dir and %s", os.TempDir())
			}
			root, err := os.MkdirTemp(base, "pr-review-bwrap-probe-*")
			if err != nil {
				return bwrapROScratch{}, func() {}, fmt.Errorf("create probe scratch under %s: %w", base, err)
			}
			scratch := bwrapROScratch{
				Root:  root,
				Work:  filepath.Join(root, "work"),
				Tmp:   filepath.Join(root, "tmp"),
				Home:  filepath.Join(root, "home"),
				Cache: filepath.Join(root, "cache"),
			}
			for _, dir := range []string{scratch.Work, scratch.Tmp, scratch.Home, scratch.Cache} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					_ = os.RemoveAll(root)
					return bwrapROScratch{}, func() {}, err
				}
			}
			return scratch, func() { _ = os.RemoveAll(root) }, nil
		},
	}
}

// scratchPaths projects a probe scratch tree onto DirectJobPaths. Every
// cache-derived field is filled, so the probe's --setenv block names exactly
// the kind of paths a real stage uses.
func scratchPaths(s bwrapROScratch) DirectJobPaths {
	return DirectJobPaths{
		Root:           s.Root,
		WorkspaceDir:   s.Work,
		TmpDir:         s.Tmp,
		HomeDir:        s.Home,
		CacheDir:       s.Cache,
		GoBuildCache:   filepath.Join(s.Cache, "go-build"),
		GoPath:         filepath.Join(s.Cache, "go"),
		GoModCache:     filepath.Join(s.Cache, "go", "pkg", "mod"),
		NpmCacheDir:    filepath.Join(s.Cache, "npm"),
		PnpmHome:       filepath.Join(s.Cache, "pnpm"),
		YarnCacheDir:   filepath.Join(s.Cache, "yarn"),
		PipCacheDir:    filepath.Join(s.Cache, "pip"),
		PoetryCacheDir: filepath.Join(s.Cache, "poetry"),
		CargoHome:      filepath.Join(s.Cache, "cargo"),
	}
}
