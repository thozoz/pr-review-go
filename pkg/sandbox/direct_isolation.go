package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirectJobPaths is one job's fully resolved, canonical, job-private tree.
// Workspace/tmp/home are ephemeral (they live under the configured work
// root); every toolchain cache is durable and lives under the configured
// cache root so a later confinement rank can grant exactly the job's roots
// without ever granting the shared host temporary directory.
type DirectJobPaths struct {
	JobID string
	// Root is <workRoot>/jobs/<JobID>.
	Root         string
	WorkspaceDir string
	TmpDir       string
	HomeDir      string
	// CacheDir is <cacheRoot>/<JobID>.
	CacheDir       string
	GoBuildCache   string
	GoPath         string
	GoModCache     string
	NpmCacheDir    string
	PnpmHome       string
	YarnCacheDir   string
	PipCacheDir    string
	PoetryCacheDir string
	CargoHome      string

	// canonical roots recorded at provision time so Cleanup can re-check
	// containment without consulting ambient configuration.
	workRoot  string
	cacheRoot string
}

// DirectRoots is the operator-configured pair of job-private roots.
type DirectRoots struct {
	WorkRoot  string
	CacheRoot string
}

// validDirectJobID accepts only non-empty lowercase hex, so a job ID can never
// escape its parent as "..", "/" or a nested path.
func validDirectJobID(jobID string) bool {
	if jobID == "" {
		return false
	}
	for _, r := range jobID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// canonicalDirectRoot creates root if needed, then resolves it to a canonical
// absolute path (symlinks evaluated), refuses the filesystem root, and refuses
// any root at or beneath the symlink-resolved shared host temporary directory.
// The temporary-directory refusal repeats the operator-config guard at the
// enforcement boundary, so an unvalidated DirectRoots value can never smuggle
// the shared /tmp into a confinement grant (08-REVIEW WR-02).
func canonicalDirectRoot(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	canonical = filepath.Clean(canonical)
	if canonical == "/" || canonical == filepath.VolumeName(canonical)+string(os.PathSeparator) {
		return "", fmt.Errorf("refusing unsafe direct root %q", root)
	}
	tmp := filepath.Clean(os.TempDir())
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = filepath.Clean(resolved)
	}
	if canonical == tmp || strictlyBeneath(canonical, tmp) {
		return "", fmt.Errorf("refusing direct root %q: must not be the shared host temporary directory", root)
	}
	return canonical, nil
}

// provisionDirectJobDirs creates and returns the job-private tree
// <WorkRoot>/jobs/<jobID>/{workspace,tmp,home} plus
// <CacheRoot>/<jobID>/{go-build,go,pkg/mod,npm,pnpm,yarn,pip,poetry,cargo}.
// Every directory is created at mode 0700. An unconfigured or unsafe root, or
// a job ID that is not lowercase hex, fails closed before any directory that
// names the job is created.
func provisionDirectJobDirs(roots DirectRoots, jobID string) (DirectJobPaths, error) {
	var paths DirectJobPaths
	if strings.TrimSpace(roots.WorkRoot) == "" {
		return paths, fmt.Errorf("SandboxDirectWorkRoot is unconfigured")
	}
	if strings.TrimSpace(roots.CacheRoot) == "" {
		return paths, fmt.Errorf("SandboxDirectCacheDir is unconfigured")
	}
	if !validDirectJobID(jobID) {
		return paths, fmt.Errorf("invalid direct job id %q", jobID)
	}
	workRoot, err := canonicalDirectRoot(roots.WorkRoot)
	if err != nil {
		return paths, err
	}
	cacheRoot, err := canonicalDirectRoot(roots.CacheRoot)
	if err != nil {
		return paths, err
	}
	root := filepath.Join(workRoot, "jobs", jobID)
	cacheDir := filepath.Join(cacheRoot, jobID)
	paths = DirectJobPaths{
		JobID:          jobID,
		Root:           root,
		WorkspaceDir:   filepath.Join(root, "workspace"),
		TmpDir:         filepath.Join(root, "tmp"),
		HomeDir:        filepath.Join(root, "home"),
		CacheDir:       cacheDir,
		GoBuildCache:   filepath.Join(cacheDir, "go-build"),
		GoPath:         filepath.Join(cacheDir, "go"),
		GoModCache:     filepath.Join(cacheDir, "go", "pkg", "mod"),
		NpmCacheDir:    filepath.Join(cacheDir, "npm"),
		PnpmHome:       filepath.Join(cacheDir, "pnpm"),
		YarnCacheDir:   filepath.Join(cacheDir, "yarn"),
		PipCacheDir:    filepath.Join(cacheDir, "pip"),
		PoetryCacheDir: filepath.Join(cacheDir, "poetry"),
		CargoHome:      filepath.Join(cacheDir, "cargo"),
		workRoot:       workRoot,
		cacheRoot:      cacheRoot,
	}
	for _, dir := range []string{
		paths.WorkspaceDir, paths.TmpDir, paths.HomeDir,
		paths.GoBuildCache, paths.GoPath, paths.GoModCache,
		paths.NpmCacheDir, paths.PnpmHome, paths.YarnCacheDir,
		paths.PipCacheDir, paths.PoetryCacheDir, paths.CargoHome,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return DirectJobPaths{}, err
		}
	}
	return paths, nil
}

// strictlyBeneath reports whether target is a descendant of root, compared
// component-wise so "/rootA" never matches root "/root".
func strictlyBeneath(target, root string) bool {
	target, root = filepath.Clean(target), filepath.Clean(root)
	if target == root {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// removeJobDirs removes this job's work tree and cache tree, re-canonicalising
// both the targets and the configured roots first, and refusing any RemoveAll
// target that is not strictly beneath its canonical root.
func removeJobDirs(paths DirectJobPaths, roots DirectRoots) error {
	canonicalWork, err := canonicalDirectRoot(roots.WorkRoot)
	if err != nil {
		return err
	}
	canonicalCache, err := canonicalDirectRoot(roots.CacheRoot)
	if err != nil {
		return err
	}
	var firstErr error
	removeGuarded := func(target, root, label string) {
		if target == "" {
			return
		}
		if _, err := os.Lstat(target); err != nil {
			if os.IsNotExist(err) {
				return
			}
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		if !strictlyBeneath(resolved, root) {
			if firstErr == nil {
				firstErr = fmt.Errorf("refusing to remove %s %q outside configured root %q", label, resolved, root)
			}
			return
		}
		if err := os.RemoveAll(resolved); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	removeGuarded(paths.Root, canonicalWork, "job root")
	removeGuarded(paths.CacheDir, canonicalCache, "job cache")
	return firstErr
}

// Cleanup is the nil-safe wrapper over removeJobDirs for the roots recorded at
// provision time.
func (p *DirectJobPaths) Cleanup() {
	if p == nil {
		return
	}
	_ = removeJobDirs(*p, DirectRoots{WorkRoot: p.workRoot, CacheRoot: p.cacheRoot})
}

// directPrepare executes only the trusted helper inside the resolved
// confinement namespace. The helper's loopback relay can reach only this
// job's restricted UDS gateway. The argv comes from the single resolved spec,
// so stage-0 is never left unwrapped (CONFINE-01).
//
// Rank-agnostic: the bwrap-ro rank binds the helper and gateway socket into its
// private tmpfs and runs through RunBwrapStage, while the landlock rank has no
// filesystem remap — it receives the host helper/socket paths, an explicit
// --rw grant on the socket directory, and the scrubbed job-private stage env,
// then runs through runDirectStage. Stage-0 therefore works under either rank
// (11-REVIEW CR-01 / 10-REVIEW CR-02).
//
// Same-path semantics: the job workspace is bound at its own host path, so the
// in-sandbox -work value equals the host workspace path (no /work remap).
//
// Runtime destinations: with `--ro-bind / /` bwrap cannot create mount-point
// parents under the read-only host root, so both the helper and the gateway
// socket are bound into the argv's private tmpfs (BwrapROHelperPath and
// BwrapGatewaySocketPath). The gateway control tree therefore lives under the
// job root (ROOT-01) and is never itself bound into the sandbox — only the
// socket file is, read-only.
func directPrepare(ctx context.Context, timeout time.Duration, paths DirectJobPaths, spec ConfinementSpec) (*StageResult, error) {
	helper, err := ResolveSandboxHelperPath()
	if err != nil {
		return nil, err
	}
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		return nil, err
	}
	control, err := os.MkdirTemp(paths.Root, "control-*")
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
	argv, err := spec.WrapArgv(
		[]string{BwrapROHelperPath, "prepare", "-socket", BwrapGatewaySocketPath, "-work", paths.WorkspaceDir},
		paths,
		WrapOptions{GatewaySocketPath: socket, HelperPath: helper},
	)
	if err != nil {
		return nil, err
	}
	stageCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if spec.Rank == RankBwrapRO {
		return RunBwrapStage(stageCtx, "install", argv)
	}
	env := spec.Env(buildDirectStageEnv(nil, &paths), paths)
	return runDirectStage(stageCtx, timeout, paths.WorkspaceDir, "install", argv, env), nil
}
