package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
)

var (
	ErrIncompleteGitlink         = errors.New("gitlink/submodule detected: submodules are unsupported in Phase 1 (D-13)")
	ErrIncompleteGitLFS          = errors.New("git LFS pointer detected: LFS is unsupported in Phase 1 (D-13)")
	ErrInvalidCommitOID          = errors.New("commit SHA must be 40 or 64 hex characters (full OID required)")
	ErrInvalidRepoURL            = errors.New("repository clone URL must be a valid canonical GitHub HTTPS URL")
	ErrUnsafePathEntry           = errors.New("unsafe or forbidden filesystem entry in git tree")
	ErrRetrievalAuthUnavailable  = github.ErrRetrievalAuthUnavailable
)

var validRepoPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// ValidateCommitOID ensures the commit hash is a full 40-char (SHA-1) or 64-char (SHA-256) hex string.
// Short prefixes and non-hex characters are rejected (SAFE-01 encoding).
func ValidateCommitOID(sha string) error {
	shaLen := len(sha)
	if shaLen != 40 && shaLen != 64 {
		return fmt.Errorf("%w: got length %d (%q)", ErrInvalidCommitOID, shaLen, sha)
	}
	for i := 0; i < shaLen; i++ {
		c := sha[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("%w: invalid character %q at index %d", ErrInvalidCommitOID, string(c), i)
		}
	}
	return nil
}

// ValidateCloneURL validates and extracts canonical owner and repository name from a public GitHub URL.
// Rejects option-like strings, arbitrary URLs, userinfo/credentials, and non-HTTPS protocols.
func ValidateCloneURL(rawURL string) (owner, repo string, err error) {
	if rawURL == "" {
		return "", "", fmt.Errorf("%w: empty URL", ErrInvalidRepoURL)
	}
	if strings.HasPrefix(rawURL, "-") {
		return "", "", fmt.Errorf("%w: option-like URL rejected: %q", ErrInvalidRepoURL, rawURL)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("%w: parse error: %v", ErrInvalidRepoURL, err)
	}

	if u.Scheme != "https" {
		return "", "", fmt.Errorf("%w: scheme must be https, got %q", ErrInvalidRepoURL, u.Scheme)
	}
	if u.Host != "github.com" && !strings.HasSuffix(u.Host, ".github.com") {
		return "", "", fmt.Errorf("%w: host must be github.com, got %q", ErrInvalidRepoURL, u.Host)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("%w: credentials in URL are prohibited", ErrInvalidRepoURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("%w: query parameters and fragments are prohibited", ErrInvalidRepoURL)
	}

	cleanPath := path.Clean(u.Path)
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%w: expected path /<owner>/<repo>, got %q", ErrInvalidRepoURL, u.Path)
	}

	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")

	if !validRepoPattern.MatchString(owner) || !validRepoPattern.MatchString(repo) {
		return "", "", fmt.Errorf("%w: invalid repository identity %s/%s", ErrInvalidRepoURL, owner, repo)
	}

	return owner, repo, nil
}

func isWindowsReserved(name string) bool {
	base := name
	if dot := strings.Index(base, "."); dot != -1 {
		base = base[:dot]
	}
	base = strings.TrimRight(base, " ")
	base = strings.ToLower(base)
	switch base {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return true
	}
	return false
}

// ValidateSnapshotPath ensures an entry path is safe, normalized, and cannot escape or collide.
func ValidateSnapshotPath(relPath string) error {
	if relPath == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafePathEntry)
	}
	if strings.ContainsRune(relPath, 0) {
		return fmt.Errorf("%w: null byte in path %q", ErrUnsafePathEntry, relPath)
	}
	if strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") {
		return fmt.Errorf("%w: absolute path %q", ErrUnsafePathEntry, relPath)
	}

	normalized := strings.ReplaceAll(relPath, "\\", "/")
	rawSegments := strings.Split(normalized, "/")
	for _, seg := range rawSegments {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: traversal path %q", ErrUnsafePathEntry, relPath)
		}
	}

	clean := path.Clean(relPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: traversal path %q", ErrUnsafePathEntry, relPath)
	}

	segments := strings.Split(clean, "/")
	for _, seg := range segments {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: relative dot segment in %q", ErrUnsafePathEntry, relPath)
		}
		if strings.EqualFold(seg, ".git") {
			return fmt.Errorf("%w: path references .git directory: %q", ErrUnsafePathEntry, relPath)
		}
		if isWindowsReserved(seg) {
			return fmt.Errorf("%w: Windows-unsafe reserved device name %q in %q", ErrUnsafePathEntry, seg, relPath)
		}
		if strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, ".") {
			return fmt.Errorf("%w: Windows-unsafe trailing dot or space %q in %q", ErrUnsafePathEntry, seg, relPath)
		}
	}

	return nil
}

// ValidateSymlinkTarget verifies that a symlink target does not escape the snapshot root.
func ValidateSymlinkTarget(relPath, target string) error {
	if target == "" {
		return fmt.Errorf("%w: empty symlink target at %q", ErrUnsafePathEntry, relPath)
	}
	if strings.HasPrefix(target, "/") || strings.HasPrefix(target, "\\") {
		return fmt.Errorf("%w: absolute symlink target %q at %q", ErrUnsafePathEntry, target, relPath)
	}
	if len(target) >= 2 && target[1] == ':' {
		return fmt.Errorf("%w: Windows drive in symlink target %q at %q", ErrUnsafePathEntry, target, relPath)
	}

	dir := path.Dir(relPath)
	resolved := path.Clean(path.Join(dir, target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: escaping symlink %q -> %q", ErrUnsafePathEntry, relPath, target)
	}

	return nil
}

// ExportGitTree extracts the exact commit tree from a bare git repository into destDir
// using git ls-tree and git cat-file, completely bypassing checkout, hooks, and filters.
// Returns an error if any unsafe entries, gitlinks (submodules), or Git LFS pointers are encountered.
func ExportGitTree(ctx context.Context, gitDir, commitSHA, destDir string) (*Snapshot, error) {
	if err := ValidateCommitOID(commitSHA); err != nil {
		return nil, err
	}

	// 1. Verify exact object equality via rev-parse
	revCmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-parse", "--verify", commitSHA+"^{commit}")
	var revOut bytes.Buffer
	revCmd.Stdout = &revOut
	if err := revCmd.Run(); err != nil {
		return nil, fmt.Errorf("git rev-parse failed for %s: %w", commitSHA, err)
	}
	verifiedSHA := strings.TrimSpace(revOut.String())
	if !strings.EqualFold(verifiedSHA, commitSHA) {
		return nil, fmt.Errorf("full commit SHA mismatch: expected %s, got %s", commitSHA, verifiedSHA)
	}

	// 2. Enumerate complete tree via git ls-tree -r -z
	lsCmd := exec.CommandContext(ctx, "git", "-C", gitDir, "ls-tree", "-r", "-z", commitSHA)
	var lsOut bytes.Buffer
	lsCmd.Stdout = &lsOut
	if err := lsCmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-tree failed: %w", err)
	}

	// 3. Process entries
	rawEntries := bytes.Split(lsOut.Bytes(), []byte{0})
	manifest := make(map[string]string)

	cleanDest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cleanDest, 0755); err != nil {
		return nil, err
	}

	snapshot := &Snapshot{
		CommitSHA: commitSHA,
		SourceDir: cleanDest,
		Manifest:  manifest,
	}

	for _, entryBytes := range rawEntries {
		if len(entryBytes) == 0 {
			continue
		}
		// Format: "<mode> <type> <sha>\t<path>"
		tabIdx := bytes.IndexByte(entryBytes, '\t')
		if tabIdx == -1 {
			continue
		}
		meta := string(entryBytes[:tabIdx])
		entryPath := string(entryBytes[tabIdx+1:])

		metaParts := strings.Fields(meta)
		if len(metaParts) < 3 {
			continue
		}
		mode := metaParts[0]
		objType := metaParts[1]
		objSHA := metaParts[2]

		if err := ValidateSnapshotPath(entryPath); err != nil {
			return nil, err
		}

		targetFsPath := filepath.Join(cleanDest, filepath.FromSlash(entryPath))

		// Check for submodule/gitlink (mode 160000 or type commit)
		if mode == "160000" || objType == "commit" {
			snapshot.IsIncomplete = true
			snapshot.IncompleteReason = fmt.Sprintf("gitlink/submodule detected at %s", entryPath)
			return snapshot, fmt.Errorf("%w: %s", ErrIncompleteGitlink, entryPath)
		}

		// Check for symlink (mode 120000)
		if mode == "120000" {
			catCmd := exec.CommandContext(ctx, "git", "-C", gitDir, "cat-file", "blob", objSHA)
			var catOut bytes.Buffer
			catCmd.Stdout = &catOut
			if err := catCmd.Run(); err != nil {
				return nil, fmt.Errorf("failed to read symlink blob for %s: %w", entryPath, err)
			}
			linkTarget := string(catOut.Bytes())
			if err := ValidateSymlinkTarget(entryPath, linkTarget); err != nil {
				return nil, err
			}

			if err := os.MkdirAll(filepath.Dir(targetFsPath), 0755); err != nil {
				return nil, err
			}
			_ = os.Remove(targetFsPath)
			if err := os.Symlink(linkTarget, targetFsPath); err != nil {
				return nil, fmt.Errorf("failed to create symlink at %s: %w", targetFsPath, err)
			}
			continue
		}

		// Regular or executable file (100644 or 100755)
		if mode == "100644" || mode == "100755" {
			catCmd := exec.CommandContext(ctx, "git", "-C", gitDir, "cat-file", "blob", objSHA)
			var catOut bytes.Buffer
			catCmd.Stdout = &catOut
			if err := catCmd.Run(); err != nil {
				return nil, fmt.Errorf("failed to read blob for %s: %w", entryPath, err)
			}
			blobData := catOut.Bytes()

			// Check for Git LFS pointer
			if bytes.HasPrefix(blobData, []byte("version https://git-lfs.github.com/spec/v1\n")) {
				snapshot.IsIncomplete = true
				snapshot.IncompleteReason = fmt.Sprintf("git LFS pointer detected at %s", entryPath)
				return snapshot, fmt.Errorf("%w: %s", ErrIncompleteGitLFS, entryPath)
			}

			// Compute sha256 for manifest
			h := sha256.Sum256(blobData)
			manifest[entryPath] = hex.EncodeToString(h[:])

			if err := os.MkdirAll(filepath.Dir(targetFsPath), 0755); err != nil {
				return nil, err
			}

			fileMode := os.FileMode(0644)
			if mode == "100755" {
				fileMode = 0755
			}
			if err := os.WriteFile(targetFsPath, blobData, fileMode); err != nil {
				return nil, fmt.Errorf("failed to write snapshot file %s: %w", targetFsPath, err)
			}
			continue
		}

		return nil, fmt.Errorf("%w: forbidden mode %q at %s", ErrUnsafePathEntry, mode, entryPath)
	}

	return snapshot, nil
}

// StartLoopbackRelay starts an HTTP loopback reverse proxy on listenAddr that forwards
// all requests to the specified Unix domain socket path.
func StartLoopbackRelay(ctx context.Context, listenAddr, socketPath string) (net.Listener, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on loopback %s: %w", listenAddr, err)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "unix"
		},
		Transport: &http.Transport{
			DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(dialCtx, "unix", socketPath)
			},
		},
	}

	server := &http.Server{
		Handler: proxy,
	}

	go func() {
		_ = server.Serve(listener)
	}()

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	return listener, nil
}

// PublicGitSource provides isolated exact-commit source retrieval for public repositories.
// It enforces canonical repository origin, full commit OID verification, and network-none
// container execution with a restricted Unix data gateway.
type PublicGitSource struct {
	Backend        *PodmanBackend
	SlotManager    SlotManager
	Config         *config.Config
	GatewayBaseURL string
}

func NewPublicGitSource(backend *PodmanBackend, sm SlotManager, cfg *config.Config) *PublicGitSource {
	baseURL := "https://github.com"
	return &PublicGitSource{
		Backend:        backend,
		SlotManager:    sm,
		Config:         cfg,
		GatewayBaseURL: baseURL,
	}
}

// PrepareSource implements the SourceProvider interface.
func (s *PublicGitSource) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error) {
	// 1. Validate canonical repository URL and full commit OID (SAFE-01)
	owner, repo, err := ValidateCloneURL(cloneURL)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateCommitOID(headSHA); err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(headRef, "-") {
		return nil, nil, fmt.Errorf("%w: option-like headRef rejected: %q", ErrInvalidSnapshot, headRef)
	}

	// 2. Ensure container isolation backend and storage slots are available (D-01)
	if s.Backend == nil || s.SlotManager == nil {
		return nil, nil, ErrSourceProviderUnavailable
	}

	// 3. Acquire exclusive storage slot lease (SAFE-01 concurrency)
	jobID := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d", owner, headSHA, time.Now().UnixNano()))))[:12]
	lease, err := s.SlotManager.AcquireSlot(ctx, jobID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to acquire storage slot: %w", err)
	}

	cleanup := func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.Backend.StopAndKillJobContainers(cleanCtx, jobID)
		_ = s.SlotManager.ReleaseSlot(lease)
	}

	// 4. Start restricted Git data gateway on a dedicated Unix socket (D-05, D-09)
	gwSockPath := filepath.Join(lease.ControlDir, fmt.Sprintf("gw-%s.sock", jobID))
	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:      gwSockPath,
		RepoOwner:       owner,
		RepoName:        repo,
		UpstreamBaseURL: s.GatewayBaseURL,
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to start git gateway: %w", err)
	}
	defer func() {
		_ = gw.Close()
	}()

	// 5. Attest container runtime capabilities before executing
	if err := s.Backend.Attest(ctx); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("runtime attestation failed: %w", err)
	}

	// 6. Execute isolated source retrieval container with --network=none
	// Scratch directory inside the leased slot for bare git repository
	gitScratchDir := filepath.Join(lease.SlotDir, "git")
	if err := os.MkdirAll(gitScratchDir, 0755); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create git scratch dir: %w", err)
	}

	retrievalArgs := []string{
		"pr-review-sandbox-helper", "retrieve",
		"-socket", "/run/sandbox/gateway.sock",
		"-repo-url", fmt.Sprintf("http://127.0.0.1:8080/%s/%s", owner, repo),
		"-commit", headSHA,
		"-git-dir", "/git/repo.git",
		"-dest", "/snapshot",
		"-manifest", "/snapshot/manifest.json",
	}

	// Create a custom stage execution for retrieval
	containerName := fmt.Sprintf("pr-rev-%s-retrieval", jobID)
	podmanArgs := []string{
		"run",
		"--name", containerName,
		"--label", fmt.Sprintf("pr-review.job_id=%s", jobID),
		"--label", "pr-review.stage=retrieval",
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pid=private",
		"--ipc=none",
		"--cgroupns=private",
		"--userns=keep-id",
		"--read-only",
		"--read-only-tmpfs=false",
		"--log-driver=none",
		"--pull=never",
		fmt.Sprintf("--cpus=%f", s.Backend.cfg.CPUs),
		fmt.Sprintf("--memory=%d", s.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--memory-swap=%d", s.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--pids-limit=%d", s.Backend.cfg.PidsLimit),
		"-v", fmt.Sprintf("%s:/run/sandbox/gateway.sock:ro", gwSockPath),
		"-v", fmt.Sprintf("%s:/git:rw", gitScratchDir),
		"-v", fmt.Sprintf("%s:/snapshot:rw", lease.SnapshotDir),
		"-v", fmt.Sprintf("%s:/tmp:rw", lease.TmpDir),
		"-w", "/tmp",
		"--entrypoint=",
		"-e", "GIT_CONFIG_NOSYSTEM=1",
		"-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-e", "GIT_CONFIG_SYSTEM=/dev/null",
		"-e", "GIT_TEMPLATE_DIR=/dev/null",
		"-e", "HOME=/tmp",
		"-e", "TMPDIR=/tmp",
		s.Backend.cfg.ImageDigest,
	}
	podmanArgs = append(podmanArgs, retrievalArgs...)

	retrievalTimeout := 2 * time.Minute
	if s.Config != nil && s.Config.SandboxTimeoutSource > 0 {
		retrievalTimeout = s.Config.SandboxTimeoutSource
	}
	retrievalCtx, cancelRetrieval := context.WithTimeout(ctx, retrievalTimeout)
	defer cancelRetrieval()

	cmd := exec.CommandContext(retrievalCtx, s.Backend.cfg.BinaryPath, podmanArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// Immediately terminate and remove retrieval container
	cleanCtx, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	_ = s.Backend.StopAndKillJobContainers(cleanCtx, jobID)

	if runErr != nil {
		exitCode := 1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}

		// Exit code 2 indicates incomplete content (gitlinks/submodules or Git LFS)
		if exitCode == 2 || strings.Contains(stderr.String(), "INCOMPLETE:") {
			reason := strings.TrimSpace(stderr.String())
			cleanup()
			return &Snapshot{
				CommitSHA:        headSHA,
				SourceDir:        lease.SnapshotDir,
				IsIncomplete:     true,
				IncompleteReason: reason,
			}, func() {}, fmt.Errorf("%w: %s", ErrIncompleteGitlink, reason)
		}

		cleanup()
		return nil, nil, fmt.Errorf("source retrieval container failed (exit %d): %v (stderr: %s)", exitCode, runErr, stderr.String())
	}

	// 7. Read and validate snapshot manifest and filesystem
	manifest := make(map[string]string)
	manifestPath := filepath.Join(lease.SnapshotDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var mf struct {
			CommitSHA string            `json:"commit_sha"`
			Manifest  map[string]string `json:"manifest"`
		}
		if jsonErr := json.Unmarshal(data, &mf); jsonErr == nil && mf.Manifest != nil {
			manifest = mf.Manifest
		}
		_ = os.Remove(manifestPath)
	}

	snapshot := &Snapshot{
		CommitSHA: headSHA,
		SourceDir: lease.SnapshotDir,
		Manifest:  manifest,
	}

	if err := snapshot.Validate(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("prepared snapshot validation failed: %w", err)
	}

	return snapshot, cleanup, nil
}

// RetrievalCredentialProvider defines the typed contract for minting and revoking short-lived,
// least-privilege App credentials for private repository source preparation (D-10, SAFE-01).
type RetrievalCredentialProvider interface {
	CreateRetrievalCredential(ctx context.Context, owner, repo string, repoID int64) (*github.RetrievalCredential, error)
	RevokeRetrievalCredential(ctx context.Context, cred *github.RetrievalCredential) error
}

// PrivateGitSource provides isolated exact-commit source retrieval for private repositories
// using a scoped, short-lived GitHub App retrieval credential (D-10, D-11).
// The credential is held strictly by the Git gateway, injected only on canonical same-repo
// read requests, and never exposed to container environment, arguments, config, or snapshots.
// The credential lease is revoked before snapshot handoff.
type PrivateGitSource struct {
	Backend        *PodmanBackend
	SlotManager    SlotManager
	Config         *config.Config
	GatewayBaseURL string
	Auth           RetrievalCredentialProvider
	HeadRepoID     int64
}

// NewPrivateGitSource creates a PrivateGitSource configured with container backend, storage slots,
// and a scoped GitHub App credential provider.
func NewPrivateGitSource(backend *PodmanBackend, sm SlotManager, cfg *config.Config, auth RetrievalCredentialProvider, headRepoID int64) *PrivateGitSource {
	baseURL := "https://github.com"
	return &PrivateGitSource{
		Backend:        backend,
		SlotManager:    sm,
		Config:         cfg,
		GatewayBaseURL: baseURL,
		Auth:           auth,
		HeadRepoID:     headRepoID,
	}
}

// PrepareSource implements the SourceProvider interface for private repositories.
func (s *PrivateGitSource) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error) {
	// 1. Validate canonical repository URL and full commit OID (SAFE-01)
	owner, repo, err := ValidateCloneURL(cloneURL)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateCommitOID(headSHA); err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(headRef, "-") {
		return nil, nil, fmt.Errorf("%w: option-like headRef rejected: %q", ErrInvalidSnapshot, headRef)
	}

	// 2. Ensure container isolation backend and storage slots are available (D-01)
	if s.Backend == nil || s.SlotManager == nil {
		return nil, nil, ErrSourceProviderUnavailable
	}

	// 3. Ensure narrow credential provider is configured (D-10)
	if s.Auth == nil {
		return nil, nil, ErrRetrievalAuthUnavailable
	}

	// 4. Acquire exclusive storage slot lease (SAFE-01 concurrency)
	jobID := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s-%s-%d", owner, headSHA, time.Now().UnixNano()))))[:12]
	lease, err := s.SlotManager.AcquireSlot(ctx, jobID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to acquire storage slot: %w", err)
	}

	// 5. Create scoped, short-lived App retrieval token for this head repo only (D-10)
	cred, err := s.Auth.CreateRetrievalCredential(ctx, owner, repo, s.HeadRepoID)
	if err != nil {
		_ = s.SlotManager.ReleaseSlot(lease)
		return nil, nil, fmt.Errorf("failed to create retrieval credential for %s/%s: %w", owner, repo, err)
	}

	var credRevoked bool
	revokeToken := func() {
		if !credRevoked && cred != nil {
			credRevoked = true
			cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = s.Auth.RevokeRetrievalCredential(cleanCtx, cred)
		}
	}

	cleanup := func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		revokeToken()
		_ = s.Backend.StopAndKillJobContainers(cleanCtx, jobID)
		_ = s.SlotManager.ReleaseSlot(lease)
	}

	// 6. Start restricted Git data gateway on a dedicated Unix socket (D-05, D-09, D-10)
	// The gateway owns the token; it is NEVER passed to container specs or files.
	gwSockPath := filepath.Join(lease.ControlDir, fmt.Sprintf("gw-%s.sock", jobID))
	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:      gwSockPath,
		RepoOwner:       owner,
		RepoName:        repo,
		UpstreamBaseURL: s.GatewayBaseURL,
		RetrievalToken:  cred.Token,
		OnClose:         revokeToken,
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to start git gateway: %w", err)
	}
	defer func() {
		_ = gw.Close()
		// Always revoke credential before snapshot handoff (D-10, key_links)
		revokeToken()
	}()

	// 7. Attest container runtime capabilities before executing
	if err := s.Backend.Attest(ctx); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("runtime attestation failed: %w", err)
	}

	// 8. Execute isolated source retrieval container with --network=none
	gitScratchDir := filepath.Join(lease.SlotDir, "git")
	if err := os.MkdirAll(gitScratchDir, 0755); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("failed to create git scratch dir: %w", err)
	}

	retrievalArgs := []string{
		"pr-review-sandbox-helper", "retrieve",
		"-socket", "/run/sandbox/gateway.sock",
		"-repo-url", fmt.Sprintf("http://127.0.0.1:8080/%s/%s", owner, repo),
		"-commit", headSHA,
		"-git-dir", "/git/repo.git",
		"-dest", "/snapshot",
		"-manifest", "/snapshot/manifest.json",
	}

	containerName := fmt.Sprintf("pr-rev-%s-retrieval", jobID)
	podmanArgs := []string{
		"run",
		"--name", containerName,
		"--label", fmt.Sprintf("pr-review.job_id=%s", jobID),
		"--label", "pr-review.stage=retrieval",
		"--network=none",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--pid=private",
		"--ipc=none",
		"--cgroupns=private",
		"--userns=keep-id",
		"--read-only",
		"--read-only-tmpfs=false",
		"--log-driver=none",
		"--pull=never",
		fmt.Sprintf("--cpus=%f", s.Backend.cfg.CPUs),
		fmt.Sprintf("--memory=%d", s.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--memory-swap=%d", s.Backend.cfg.MemoryBytes),
		fmt.Sprintf("--pids-limit=%d", s.Backend.cfg.PidsLimit),
		"-v", fmt.Sprintf("%s:/run/sandbox/gateway.sock:ro", gwSockPath),
		"-v", fmt.Sprintf("%s:/git:rw", gitScratchDir),
		"-v", fmt.Sprintf("%s:/snapshot:rw", lease.SnapshotDir),
		"-v", fmt.Sprintf("%s:/tmp:rw", lease.TmpDir),
		"-w", "/tmp",
		"--entrypoint=",
		"-e", "GIT_CONFIG_NOSYSTEM=1",
		"-e", "GIT_CONFIG_GLOBAL=/dev/null",
		"-e", "GIT_CONFIG_SYSTEM=/dev/null",
		"-e", "GIT_TEMPLATE_DIR=/dev/null",
		"-e", "HOME=/tmp",
		"-e", "TMPDIR=/tmp",
		s.Backend.cfg.ImageDigest,
	}
	podmanArgs = append(podmanArgs, retrievalArgs...)

	retrievalTimeout := 2 * time.Minute
	if s.Config != nil && s.Config.SandboxTimeoutSource > 0 {
		retrievalTimeout = s.Config.SandboxTimeoutSource
	}
	retrievalCtx, cancelRetrieval := context.WithTimeout(ctx, retrievalTimeout)
	defer cancelRetrieval()

	cmd := exec.CommandContext(retrievalCtx, s.Backend.cfg.BinaryPath, podmanArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// Immediately terminate and remove retrieval container
	cleanCtx, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	_ = s.Backend.StopAndKillJobContainers(cleanCtx, jobID)

	// Explicitly close gateway and revoke token NOW before touching or handing off snapshot
	_ = gw.Close()
	revokeToken()

	if runErr != nil {
		exitCode := 1
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}

		if exitCode == 2 || strings.Contains(stderr.String(), "INCOMPLETE:") {
			reason := strings.TrimSpace(stderr.String())
			cleanup()
			return &Snapshot{
				CommitSHA:        headSHA,
				SourceDir:        lease.SnapshotDir,
				IsIncomplete:     true,
				IncompleteReason: reason,
			}, func() {}, fmt.Errorf("%w: %s", ErrIncompleteGitlink, reason)
		}

		cleanup()
		return nil, nil, fmt.Errorf("source retrieval container failed (exit %d): %v (stderr: %s)", exitCode, runErr, stderr.String())
	}

	// 9. Read and validate snapshot manifest and filesystem
	manifest := make(map[string]string)
	manifestPath := filepath.Join(lease.SnapshotDir, "manifest.json")
	if data, err := os.ReadFile(manifestPath); err == nil {
		var mf struct {
			CommitSHA string            `json:"commit_sha"`
			Manifest  map[string]string `json:"manifest"`
		}
		if jsonErr := json.Unmarshal(data, &mf); jsonErr == nil && mf.Manifest != nil {
			manifest = mf.Manifest
		}
		_ = os.Remove(manifestPath)
	}

	snapshot := &Snapshot{
		CommitSHA: headSHA,
		SourceDir: lease.SnapshotDir,
		Manifest:  manifest,
	}

	if err := snapshot.Validate(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("prepared snapshot validation failed: %w", err)
	}

	return snapshot, cleanup, nil
}
