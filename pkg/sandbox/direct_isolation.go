package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// directIsolatedArgv creates a private root with no host home, root, or sockets.
// Only job-owned directories are writable. Toolchain roots are read-only.
func directIsolatedArgv(work, cache, tmp string, command []string, socket, helper string) ([]string, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: direct execution requires Linux mount isolation", ErrBwrapUnavailable)
	}
	argv := []string{"bwrap", "--unshare-all", "--die-with-parent", "--new-session"}
	for _, path := range []string{"/usr/bin", "/usr/lib", "/usr/lib64", "/usr/libexec", "/usr/include", "/usr/share", "/bin", "/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			argv = append(argv, "--ro-bind", path, path)
		}
	}
	goroot, err := filepath.EvalSymlinks(runtime.GOROOT())
	if err != nil {
		return nil, fmt.Errorf("trusted Go toolchain unavailable: %w", err)
	}
	if goroot == "/" || strings.HasPrefix(goroot, filepath.Clean(work)+"/") {
		return nil, fmt.Errorf("unsafe Go toolchain mount")
	}
	argv = append(argv, "--ro-bind", goroot, goroot, "--proc", "/proc", "--dev", "/dev")
	for _, mount := range [][2]string{{work, "/work"}, {cache, "/cache"}, {tmp, "/tmp"}} {
		source, err := filepath.EvalSymlinks(mount[0])
		if err != nil {
			return nil, err
		}
		if source == "/" {
			return nil, fmt.Errorf("host root mount refused")
		}
		argv = append(argv, "--bind", source, mount[1])
	}
	if socket != "" {
		if helper == "" {
			return nil, fmt.Errorf("trusted preparation helper unavailable")
		}
		argv = append(argv, "--ro-bind", socket, BwrapGatewaySocketPath, "--ro-bind", helper, "/sandbox-helper")
	}
	argv = append(argv, "--clearenv", "--setenv", "PATH", goroot+"/bin:/usr/bin:/bin", "--setenv", "HOME", "/tmp", "--setenv", "PWD", "/work",
		"--setenv", "GOPROXY", "off", "--setenv", "GOTOOLCHAIN", "local", "--setenv", "GOVCS", "off", "--setenv", "GOWORK", "off", "--setenv", "GOENV", "off",
		"--setenv", "GOROOT", goroot, "--setenv", "GO111MODULE", "on",
		"--setenv", "GOCACHE", "/cache/go-build", "--setenv", "GOPATH", "/cache/go", "--setenv", "GOMODCACHE", "/cache/go/pkg/mod", "--setenv", "TMPDIR", "/tmp", "--chdir", "/work")
	return append(argv, command...), nil
}

func directJobDirs() (string, string, func(), error) {
	root, err := os.MkdirTemp("", "pr-review-private-*")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	cache, tmp := filepath.Join(root, "cache"), filepath.Join(root, "tmp")
	for _, dir := range []string{cache, tmp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			cleanup()
			return "", "", nil, err
		}
	}
	return cache, tmp, cleanup, nil
}

// directPrepare executes only the trusted helper inside the isolated namespace.
// The helper's loopback relay can reach only this job's restricted UDS gateway.
func directPrepare(ctx context.Context, timeout time.Duration, work, cache, tmp string) (*StageResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	helper := filepath.Join(filepath.Dir(exe), "pr-review-sandbox-helper")
	if fi, err := os.Stat(helper); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("trusted sibling pr-review-sandbox-helper unavailable")
	}
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		return nil, err
	}
	control, err := os.MkdirTemp("", "pr-review-gateway-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(control)
	socket := filepath.Join(control, "gateway.sock")
	gw, err := StartDependencyGateway(DependencyGatewayConfig{SocketPath: socket, RequestTimeout: timeout})
	if err != nil {
		return nil, err
	}
	defer gw.Close()
	argv, err := directIsolatedArgv(work, cache, tmp, []string{"/sandbox-helper", "prepare", "-socket", BwrapGatewaySocketPath, "-work", "/work"}, socket, helper)
	if err != nil {
		return nil, err
	}
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return RunBwrapStage(stageCtx, "install", argv)
}
