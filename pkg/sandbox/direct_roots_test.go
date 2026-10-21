package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// provisionTestJobPaths provisions a job tree under two throwaway roots and
// registers its cleanup. Shared by the root tests and the Linux-only
// isolation tests that previously used directJobDirs.
func provisionTestJobPaths(t *testing.T) DirectJobPaths {
	t.Helper()
	paths, err := provisionDirectJobDirs(
		DirectRoots{WorkRoot: t.TempDir(), CacheRoot: t.TempDir()},
		"0123456789ab",
	)
	if err != nil {
		t.Fatalf("provision test job paths: %v", err)
	}
	t.Cleanup(paths.Cleanup)
	return paths
}

// newTestDirectRunner builds a DirectRunner bound to throwaway job-private
// roots so direct-mode tests exercise the rooted path instead of the
// fail-closed unconfigured-roots path.
func newTestDirectRunner(t *testing.T, timeout time.Duration) *DirectRunner {
	t.Helper()
	return NewDirectRunnerWithRoots(timeout, DirectRoots{WorkRoot: t.TempDir(), CacheRoot: t.TempDir()})
}

func TestDirectJobPathsLayout(t *testing.T) {
	workRoot := t.TempDir()
	cacheRoot := t.TempDir()
	paths, err := provisionDirectJobDirs(DirectRoots{WorkRoot: workRoot, CacheRoot: cacheRoot}, "0123456789ab")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(paths.Cleanup)

	if paths.JobID != "0123456789ab" {
		t.Fatalf("JobID = %q, want 0123456789ab", paths.JobID)
	}
	if got, want := paths.WorkspaceDir, filepath.Join(paths.Root, "workspace"); got != want {
		t.Errorf("WorkspaceDir = %q, want %q", got, want)
	}
	if got, want := paths.TmpDir, filepath.Join(paths.Root, "tmp"); got != want {
		t.Errorf("TmpDir = %q, want %q", got, want)
	}
	if got, want := paths.HomeDir, filepath.Join(paths.Root, "home"); got != want {
		t.Errorf("HomeDir = %q, want %q", got, want)
	}
	if !strictlyBeneath(paths.CacheDir, paths.cacheRoot) {
		t.Errorf("CacheDir %q is not beneath cache root %q", paths.CacheDir, paths.cacheRoot)
	}
	if strictlyBeneath(paths.CacheDir, paths.workRoot) {
		t.Errorf("CacheDir %q must not sit beneath the ephemeral work root %q", paths.CacheDir, paths.workRoot)
	}
	// The cache is a durable warm tree, the work area is ephemeral: the two
	// configured roots must stay independent.
	if paths.workRoot == paths.cacheRoot {
		t.Fatalf("work and cache roots collapsed to %q", paths.workRoot)
	}
	if filepath.Base(paths.Root) != paths.JobID {
		t.Errorf("work leaf %q does not round-trip JobID", filepath.Base(paths.Root))
	}
	if filepath.Base(paths.CacheDir) != paths.JobID {
		t.Errorf("cache leaf %q does not round-trip JobID", filepath.Base(paths.CacheDir))
	}
	for _, dir := range []string{
		paths.Root, paths.WorkspaceDir, paths.TmpDir, paths.HomeDir,
		paths.CacheDir, paths.GoBuildCache, paths.GoPath, paths.GoModCache,
		paths.NpmCacheDir, paths.PnpmHome, paths.YarnCacheDir,
		paths.PipCacheDir, paths.PoetryCacheDir, paths.CargoHome,
	} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Errorf("expected directory %q: %v", dir, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%q is not a directory", dir)
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%q mode = %o, want 0700", dir, perm)
		}
	}
}

func TestDirectJobPathsDoNotUseHostTmp(t *testing.T) {
	// Root provisioning must be independent of the server's TMPDIR: point
	// TMPDIR at an unrelated directory and prove no resolved path lands there.
	workRoot := t.TempDir()
	cacheRoot := t.TempDir()
	unrelatedTmp := t.TempDir()
	t.Setenv("TMPDIR", unrelatedTmp)

	paths, err := provisionDirectJobDirs(DirectRoots{WorkRoot: workRoot, CacheRoot: cacheRoot}, "aabbccdd1122")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(paths.Cleanup)

	fields := map[string]string{
		"Root": paths.Root, "WorkspaceDir": paths.WorkspaceDir, "TmpDir": paths.TmpDir,
		"HomeDir": paths.HomeDir, "CacheDir": paths.CacheDir,
		"GoBuildCache": paths.GoBuildCache, "GoPath": paths.GoPath, "GoModCache": paths.GoModCache,
		"NpmCacheDir": paths.NpmCacheDir, "PnpmHome": paths.PnpmHome, "YarnCacheDir": paths.YarnCacheDir,
		"PipCacheDir": paths.PipCacheDir, "PoetryCacheDir": paths.PoetryCacheDir, "CargoHome": paths.CargoHome,
	}
	checkIndependent := func(label, value string) {
		if value == "" {
			t.Errorf("%s is empty", label)
			return
		}
		for _, shared := range []string{unrelatedTmp, os.TempDir()} {
			if shared == "" {
				continue
			}
			if value == shared || strings.HasPrefix(value, shared+string(os.PathSeparator)) {
				t.Errorf("%s %q resolves under the shared host TMPDIR %q", label, value, shared)
			}
		}
	}
	for label, value := range fields {
		checkIndependent(label, value)
	}
	for _, kv := range buildDirectStageEnv(nil, &paths) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			checkIndependent("stage env "+kv[:i], kv[i+1:])
		}
	}
}

func TestDirectJobPathsCleanup(t *testing.T) {
	workRoot := t.TempDir()
	cacheRoot := t.TempDir()
	paths, err := provisionDirectJobDirs(DirectRoots{WorkRoot: workRoot, CacheRoot: cacheRoot}, "0123456789ab")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(paths.WorkspaceDir, "sentinel"), []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.CacheDir, "sentinel"), []byte("c"), 0o600); err != nil {
		t.Fatal(err)
	}

	paths.Cleanup()

	if _, err := os.Stat(paths.Root); !os.IsNotExist(err) {
		t.Errorf("job root %q still exists after Cleanup (err=%v)", paths.Root, err)
	}
	if _, err := os.Stat(paths.CacheDir); !os.IsNotExist(err) {
		t.Errorf("job cache %q still exists after Cleanup (err=%v)", paths.CacheDir, err)
	}
	for _, root := range []string{workRoot, cacheRoot} {
		if _, err := os.Stat(root); err != nil {
			t.Errorf("configured root %q must survive Cleanup: %v", root, err)
		}
	}
}

func TestDirectJobPathsRefusesUnsafeRoot(t *testing.T) {
	if _, err := provisionDirectJobDirs(DirectRoots{CacheRoot: t.TempDir()}, "0123456789ab"); err == nil {
		t.Error("empty WorkRoot must be refused")
	}
	if _, err := provisionDirectJobDirs(DirectRoots{WorkRoot: t.TempDir()}, "0123456789ab"); err == nil {
		t.Error("empty CacheRoot must be refused")
	}

	workRoot := t.TempDir()
	if _, err := provisionDirectJobDirs(DirectRoots{WorkRoot: workRoot, CacheRoot: t.TempDir()}, "../escape"); err == nil {
		t.Error("job id with parent-directory traversal must be refused")
	}
	if _, err := os.Stat(filepath.Join(workRoot, "jobs")); !os.IsNotExist(err) {
		t.Errorf("refused job id must create nothing beneath the root (err=%v)", err)
	}
}

func TestBuildDirectStageEnvIsolatesRoots(t *testing.T) {
	paths := provisionTestJobPaths(t)

	// Set sentinels only AFTER provisioning: t.TempDir() consults TMPDIR.
	sentinels := map[string]string{
		"HOME":              "/server/HOME",
		"TMPDIR":            "/server/TMPDIR",
		"GOTMPDIR":          "/server/GOTMPDIR",
		"GOCACHE":           "/server/GOCACHE",
		"GOPATH":            "/server/GOPATH",
		"GOMODCACHE":        "/server/GOMODCACHE",
		"XDG_CACHE_HOME":    "/server/XDG_CACHE_HOME",
		"npm_config_cache":  "/server/npm_config_cache",
		"PNPM_HOME":         "/server/PNPM_HOME",
		"YARN_CACHE_FOLDER": "/server/YARN_CACHE_FOLDER",
		"PIP_CACHE_DIR":     "/server/PIP_CACHE_DIR",
		"POETRY_CACHE_DIR":  "/server/POETRY_CACHE_DIR",
		"CARGO_HOME":        "/server/CARGO_HOME",
	}
	for k, v := range sentinels {
		t.Setenv(k, v)
	}

	env := buildDirectStageEnv(
		[]string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOVCS=off", "GOENV=off"},
		&paths,
	)

	want := map[string]string{
		"HOME":              paths.HomeDir,
		"TMPDIR":            paths.TmpDir,
		"GOTMPDIR":          paths.TmpDir,
		"GOCACHE":           paths.GoBuildCache,
		"GOPATH":            paths.GoPath,
		"GOMODCACHE":        paths.GoModCache,
		"XDG_CACHE_HOME":    paths.CacheDir,
		"npm_config_cache":  paths.NpmCacheDir,
		"PNPM_HOME":         paths.PnpmHome,
		"YARN_CACHE_FOLDER": paths.YarnCacheDir,
		"PIP_CACHE_DIR":     paths.PipCacheDir,
		"POETRY_CACHE_DIR":  paths.PoetryCacheDir,
		"CARGO_HOME":        paths.CargoHome,
	}
	for key, wantValue := range want {
		var values []string
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == key {
				values = append(values, kv[i+1:])
			}
		}
		if len(values) != 1 {
			t.Errorf("root key %q must appear exactly once, got %d (%v)", key, len(values), values)
			continue
		}
		if values[0] != wantValue {
			t.Errorf("root key %q = %q, want job-private %q", key, values[0], wantValue)
		}
	}
	goproxyCount := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GOPROXY=") {
			goproxyCount++
		}
		if strings.Contains(kv, "/server/") {
			t.Errorf("inherited /server/ sentinel survived into stage env: %q", kv)
		}
	}
	if goproxyCount != 1 {
		t.Errorf("GOPROXY must appear exactly once, got %d", goproxyCount)
	}
}

func TestBuildDirectStageEnvDropsInheritedRootKeys(t *testing.T) {
	t.Setenv("MYAPP_FOO", "innocent")
	t.Setenv("HOME", "/server/HOME")

	env := buildDirectStageEnv(nil, nil)
	foundInnocent := false
	for _, kv := range env {
		if kv == "MYAPP_FOO=innocent" {
			foundInnocent = true
		}
		if strings.HasPrefix(kv, "HOME=") {
			t.Errorf("inherited HOME must be dropped even without job paths: %q", kv)
		}
	}
	if !foundInnocent {
		t.Error("a non-root, non-secret key must survive the scrub")
	}
}

// TestBuildDirectStageEnvDropsAllGoOverrides proves the pinned offline Go
// list is authoritative: every inherited override key is dropped before the
// pins are appended, so a first-match getenv can never resolve to the server's
// ambient value under the landlock rank (11-REVIEW WR-02 / T-261021-01).
func TestBuildDirectStageEnvDropsAllGoOverrides(t *testing.T) {
	paths := provisionTestJobPaths(t)

	inherited := map[string]string{
		"GOTOOLCHAIN": "auto",
		"GOWORK":      "/tmp/ambient",
		"GOVCS":       "*:all",
		"GOENV":       "/tmp/evil.env",
		"GO111MODULE": "off",
	}
	for k, v := range inherited {
		t.Setenv(k, v)
	}

	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/envdrops\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	proj := detectDirectProject(dir)
	if proj == nil || proj.lang != "go" {
		t.Fatalf("detectDirectProject(%q) = %+v, want a go project", dir, proj)
	}

	env := buildDirectStageEnv(proj.offline, &paths)
	pinned := map[string]string{
		"GOTOOLCHAIN": "local",
		"GOWORK":      "off",
		"GOVCS":       "off",
		"GOENV":       "off",
		"GO111MODULE": "on",
	}
	for key, want := range pinned {
		var values []string
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == key {
				values = append(values, kv[i+1:])
			}
		}
		if len(values) != 1 {
			t.Errorf("%s must appear exactly once (inherited value dropped), got %d (%v)", key, len(values), values)
			continue
		}
		if values[0] != want {
			t.Errorf("%s = %q, want pinned offline %q (inherited %q must not win)", key, values[0], want, inherited[key])
		}
	}
	for _, kv := range env {
		for key, inheritedValue := range inherited {
			if kv == key+"="+inheritedValue {
				t.Errorf("inherited %s=%s survived into the stage env: %q", key, inheritedValue, kv)
			}
		}
	}
}

func TestDirectRunnerUnconfiguredRootsFailsClosed(t *testing.T) {
	if err := AttestBwrap(context.Background()); err != nil {
		t.Skipf("direct enforcement requires bubblewrap: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
	dir := writeDirectModule(t, map[string]string{
		"go.mod":  "module example.com/unconfigured\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	d := &DirectRunner{Timeout: time.Minute} // empty WorkRoot and CacheRoot
	report, err := d.VerifyDirGated(context.Background(), dir, directTestSHA, false)
	if err != nil {
		t.Fatalf("VerifyDirGated returned caller error: %v", err)
	}
	if report.Status == StatusPassed {
		t.Fatal("unconfigured roots must never yield a pass")
	}
	if report.Status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", report.Status, StatusUnavailable)
	}
	if !strings.Contains(report.Reason, "direct job roots unavailable") {
		t.Errorf("reason must name the unconfigured roots: %q", report.Reason)
	}
	if len(report.Results) != 0 {
		t.Errorf("no command may run with unconfigured roots, got %d results", len(report.Results))
	}
}
