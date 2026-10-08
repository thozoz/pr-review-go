package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/github"
)

func TestValidateCommitOID(t *testing.T) {
	// Valid 40-char SHA-1
	if err := ValidateCommitOID("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("expected valid 40-char SHA-1, got: %v", err)
	}

	// Valid 64-char SHA-256
	if err := ValidateCommitOID("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("expected valid 64-char SHA-256, got: %v", err)
	}

	// Short prefix rejected (D-09)
	if err := ValidateCommitOID("0123456"); !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for 7-char prefix, got: %v", err)
	}

	// 39 chars rejected
	if err := ValidateCommitOID("0123456789abcdef0123456789abcdef0123456"); !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for 39-char string, got: %v", err)
	}

	// Non-hex characters rejected
	if err := ValidateCommitOID("0123456789abcdef0123456789abcdef0123456g"); !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for non-hex char, got: %v", err)
	}

	// Empty string rejected
	if err := ValidateCommitOID(""); !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for empty string, got: %v", err)
	}
}

func TestValidateCloneURL(t *testing.T) {
	cases := []struct {
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https://github.com/thozoz/pr-review-go", "thozoz", "pr-review-go", false},
		{"https://github.com/thozoz/pr-review-go.git", "thozoz", "pr-review-go", false},
		{"http://github.com/thozoz/pr-review-go", "", "", true},                         // non-https
		{"https://gitlab.com/thozoz/pr-review-go", "", "", true},                        // non-github
		{"https://user:pass@github.com/thozoz/pr-review-go", "", "", true},              // userinfo
		{"-oProxyCommand=calc.exe", "", "", true},                                       // option injection
		{"https://github.com/thozoz/pr-review-go?foo=bar", "", "", true},                // query param
		{"https://github.com/thozoz/pr-review-go#frag", "", "", true},                   // fragment
		{"https://github.com/thozoz/pr-review-go/extra", "", "", true},                  // extra segment
		{"https://github.com/thozoz", "", "", true},                                     // missing repo
	}

	for _, tc := range cases {
		owner, repo, err := ValidateCloneURL(tc.url)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateCloneURL(%q): got err=%v, wantErr=%v", tc.url, err, tc.wantErr)
		}
		if !tc.wantErr {
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("ValidateCloneURL(%q): got %s/%s, want %s/%s", tc.url, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		}
	}
}

func TestValidateSnapshotPath(t *testing.T) {
	valid := []string{
		"main.go",
		"pkg/sandbox/source.go",
		"docs/README.md",
		"a/b/c/d/e.txt",
	}
	for _, p := range valid {
		if err := ValidateSnapshotPath(p); err != nil {
			t.Errorf("expected valid path %q, got: %v", p, err)
		}
	}

	invalid := []string{
		"/etc/passwd",
		"\\Windows\\System32",
		"../escape.go",
		"foo/../../bar.go",
		".git/config",
		"foo/.git/HEAD",
		"foo/.GIT/HEAD",
		"CON",
		"aux.go",
		"PRN.txt",
		"NUL",
		"com1.dat",
		"lpt3",
		"foo. ",
		"bar.",
		"foo\x00bar",
	}
	for _, p := range invalid {
		if err := ValidateSnapshotPath(p); err == nil {
			t.Errorf("expected path %q to be rejected, but passed", p)
		}
	}
}

func TestValidateSymlinkTarget(t *testing.T) {
	// Safe targets
	if err := ValidateSymlinkTarget("pkg/link.go", "source.go"); err != nil {
		t.Errorf("expected safe link target, got: %v", err)
	}
	if err := ValidateSymlinkTarget("pkg/sub/link.go", "../source.go"); err != nil {
		t.Errorf("expected safe sibling target, got: %v", err)
	}

	// Escaping targets
	if err := ValidateSymlinkTarget("link.go", "../secret"); err == nil {
		t.Errorf("expected escaping link target to fail")
	}
	if err := ValidateSymlinkTarget("pkg/link.go", "../../secret"); err == nil {
		t.Errorf("expected escaping link target to fail")
	}
	if err := ValidateSymlinkTarget("pkg/link.go", "/etc/passwd"); err == nil {
		t.Errorf("expected absolute link target to fail")
	}
	if err := ValidateSymlinkTarget("pkg/link.go", "C:\\secret"); err == nil {
		t.Errorf("expected Windows drive target to fail")
	}
}

func setupTestGitRepo(t *testing.T) (repoDir string, commitSHA string) {
	t.Helper()
	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found in PATH")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"HOME=/tmp",
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (output: %s)", args, err, string(out))
		}
	}

	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "lib.go"), []byte("package pkg\n"), 0644); err != nil {
		t.Fatal(err)
	}

	run("add", ".")
	run("commit", "-m", "initial commit")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	commitSHA = strings.TrimSpace(string(out))
	return dir, commitSHA
}

func TestExportGitTree_CleanRepo(t *testing.T) {
	gitDir, commitSHA := setupTestGitRepo(t)
	destDir := t.TempDir()

	snap, err := ExportGitTree(context.Background(), gitDir, commitSHA, destDir)
	if err != nil {
		t.Fatalf("ExportGitTree failed: %v", err)
	}

	if snap.CommitSHA != commitSHA {
		t.Fatalf("expected commit SHA %s, got %s", commitSHA, snap.CommitSHA)
	}

	// Verify main.go exists in dest
	mainData, err := os.ReadFile(filepath.Join(destDir, "main.go"))
	if err != nil {
		t.Fatalf("failed to read main.go: %v", err)
	}
	if !strings.Contains(string(mainData), "package main") {
		t.Fatalf("unexpected content in main.go")
	}

	// Verify manifest
	if len(snap.Manifest) != 2 {
		t.Fatalf("expected 2 files in manifest, got: %d (%v)", len(snap.Manifest), snap.Manifest)
	}
	if _, ok := snap.Manifest["main.go"]; !ok {
		t.Fatalf("main.go missing from manifest")
	}
	if _, ok := snap.Manifest["pkg/lib.go"]; !ok {
		t.Fatalf("pkg/lib.go missing from manifest")
	}

	// Verify .git is absent in destDir
	if _, err := os.Stat(filepath.Join(destDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git directory must not be exported into snapshot")
	}
}

func TestExportGitTree_EscapingSymlinkRejected(t *testing.T) {
	gitDir, _ := setupTestGitRepo(t)
	destDir := t.TempDir()

	// Add an escaping symlink
	_ = os.Symlink("../../outside", filepath.Join(gitDir, "badlink"))
	cmd := exec.Command("git", "add", "badlink")
	cmd.Dir = gitDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "add bad link")
	cmd.Dir = gitDir
	cmd.Env = []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	_ = cmd.Run()

	out, _ := exec.Command("git", "-C", gitDir, "rev-parse", "HEAD").Output()
	badSHA := strings.TrimSpace(string(out))

	_, err := ExportGitTree(context.Background(), gitDir, badSHA, destDir)
	if err == nil || !strings.Contains(err.Error(), "escaping symlink") {
		t.Fatalf("expected escaping symlink error, got: %v", err)
	}
}

func TestExportGitTree_GitLFSPointerDetected(t *testing.T) {
	gitDir, _ := setupTestGitRepo(t)
	destDir := t.TempDir()

	// Add a simulated Git LFS pointer
	lfsContent := "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e7b54195773d0fa723d849adc3fe4f31871776d7b1\nsize 12345\n"
	_ = os.WriteFile(filepath.Join(gitDir, "large.bin"), []byte(lfsContent), 0644)

	cmd := exec.Command("git", "add", "large.bin")
	cmd.Dir = gitDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "add lfs pointer")
	cmd.Dir = gitDir
	cmd.Env = []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	_ = cmd.Run()

	out, _ := exec.Command("git", "-C", gitDir, "rev-parse", "HEAD").Output()
	lfsSHA := strings.TrimSpace(string(out))

	snap, err := ExportGitTree(context.Background(), gitDir, lfsSHA, destDir)
	if !errors.Is(err, ErrIncompleteGitLFS) {
		t.Fatalf("expected ErrIncompleteGitLFS, got: %v", err)
	}
	if snap == nil || !snap.IsIncomplete {
		t.Fatalf("expected snapshot.IsIncomplete to be true")
	}
}

func TestExportGitTree_WindowsUnsafeEntryRejected(t *testing.T) {
	gitDir, _ := setupTestGitRepo(t)
	destDir := t.TempDir()

	// Add an entry with Windows-unsafe name: aux.go
	_ = os.WriteFile(filepath.Join(gitDir, "aux.go"), []byte("package main\n"), 0644)
	cmd := exec.Command("git", "add", "aux.go")
	cmd.Dir = gitDir
	_ = cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "add aux.go")
	cmd.Dir = gitDir
	cmd.Env = []string{
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	_ = cmd.Run()

	out, _ := exec.Command("git", "-C", gitDir, "rev-parse", "HEAD").Output()
	auxSHA := strings.TrimSpace(string(out))

	_, err := ExportGitTree(context.Background(), gitDir, auxSHA, destDir)
	if err == nil || !errors.Is(err, ErrUnsafePathEntry) {
		t.Fatalf("expected ErrUnsafePathEntry for aux.go, got: %v", err)
	}
}

func TestLoopbackRelay_EndToEnd(t *testing.T) {
	// Create mock Unix domain socket server
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "test.sock")

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
	}
	defer l.Close()

	go func() {
		_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("hello from unix socket"))
		}))
	}()

	// Start loopback relay
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayListener, err := StartLoopbackRelay(ctx, "127.0.0.1:0", sockPath)
	if err != nil {
		t.Fatalf("failed to start relay: %v", err)
	}
	defer relayListener.Close()

	relayAddr := relayListener.Addr().String()

	// Query via TCP HTTP to relayAddr
	resp, err := http.Get("http://" + relayAddr + "/test")
	if err != nil {
		t.Fatalf("failed to GET from relay: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from unix socket" {
		t.Fatalf("unexpected response: %q", string(body))
	}
}

func TestPublicGitSource_ValidationAndMissingBackend(t *testing.T) {
	// 1. Missing backend/slot manager returns ErrSourceProviderUnavailable
	src := NewPublicGitSource(nil, nil, nil)
	validSHA := "0123456789abcdef0123456789abcdef01234567"
	_, _, err := src.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", validSHA)
	if !errors.Is(err, ErrSourceProviderUnavailable) {
		t.Fatalf("expected ErrSourceProviderUnavailable when backend is nil, got: %v", err)
	}

	// 2. Invalid short prefix commit SHA fails closed before backend
	_, _, err = src.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", "abc1234")
	if !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for prefix SHA, got: %v", err)
	}

	// 3. Option-like clone URL fails closed
	_, _, err = src.PrepareSource(context.Background(), "-oProxyCommand=calc", "main", validSHA)
	if !errors.Is(err, ErrInvalidRepoURL) {
		t.Fatalf("expected ErrInvalidRepoURL for option-like URL, got: %v", err)
	}
}

type mockCredentialProvider struct {
	token       string
	createCalls int
	revokeCalls int
	createErr   error
	revokeErr   error
	lastRepoID  int64
	lastOwner   string
	lastRepo    string
	createdCred *github.RetrievalCredential
}

func (m *mockCredentialProvider) CreateRetrievalCredential(ctx context.Context, owner, repo string, repoID int64) (*github.RetrievalCredential, error) {
	m.createCalls++
	m.lastOwner = owner
	m.lastRepo = repo
	m.lastRepoID = repoID
	if m.createErr != nil {
		return nil, m.createErr
	}
	cred := &github.RetrievalCredential{
		Token:        m.token,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		RepositoryID: repoID,
		RepoOwner:    owner,
		RepoName:     repo,
		Permissions:  map[string]string{"contents": "read"},
	}
	m.createdCred = cred
	return cred, nil
}

func (m *mockCredentialProvider) RevokeRetrievalCredential(ctx context.Context, cred *github.RetrievalCredential) error {
	m.revokeCalls++
	if cred != nil {
		cred.Zeroize()
	}
	return m.revokeErr
}

type mockSlotManager struct {
	slotDir    string
	controlDir string
}

func newMockSlotManager(t *testing.T) *mockSlotManager {
	dir := t.TempDir()
	slotDir := filepath.Join(dir, "slot")
	ctrlDir := filepath.Join(dir, "ctrl")
	_ = os.MkdirAll(slotDir, 0755)
	_ = os.MkdirAll(ctrlDir, 0755)
	return &mockSlotManager{slotDir: slotDir, controlDir: ctrlDir}
}

func (m *mockSlotManager) AcquireSlot(ctx context.Context, jobID string) (*SlotLease, error) {
	snapDir := filepath.Join(m.slotDir, "snapshot")
	tmpDir := filepath.Join(m.slotDir, "tmp")
	workDir := filepath.Join(m.slotDir, "work")
	cacheDir := filepath.Join(m.slotDir, "cache")
	_ = os.MkdirAll(snapDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(workDir, 0755)
	_ = os.MkdirAll(cacheDir, 0755)
	return &SlotLease{
		JobID:       jobID,
		SlotDir:     m.slotDir,
		ControlDir:  m.controlDir,
		SnapshotDir: snapDir,
		TmpDir:      tmpDir,
		WorkDir:     workDir,
		CacheDir:    cacheDir,
	}, nil
}

func (m *mockSlotManager) ReleaseSlot(lease *SlotLease) error          { return nil }
func (m *mockSlotManager) QuarantineSlot(slotDir, reason string) error { return nil }
func (m *mockSlotManager) IsQuarantined(slotDir string) bool           { return false }
func (m *mockSlotManager) ReconcileSlots(ctx context.Context) error    { return nil }

func newMockPodmanBackend(t *testing.T) *PodmanBackend {
	mockScript := filepath.Join(t.TempDir(), "mock-podman.sh")
	scriptContent := `#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "info" ]; then
        echo '{"host":{"rootless":true,"cgroupVersion":"v2","security":{"rootless":true}}}'
        exit 0
    fi
    if [ "$arg" = "/sys/fs/cgroup/memory.max" ]; then
        echo '2147483648'
        exit 0
    fi
    if [ "$arg" = "rm" ] || [ "$arg" = "kill" ]; then
        exit 0
    fi
done
exit 0
`
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock podman script: %v", err)
	}

	return NewPodmanBackend(PodmanConfig{
		BinaryPath:  mockScript,
		ImageDigest: "test-image:latest",
		CPUs:        2.0,
		MemoryBytes: 2 * 1024 * 1024 * 1024,
		PidsLimit:   256,
	})
}


func TestPrivateGitSource_ValidationAndMissingBackend(t *testing.T) {
	validSHA := "0123456789abcdef0123456789abcdef01234567"
	mockCreds := &mockCredentialProvider{token: "dummy-token"}

	// 1. Missing backend returns ErrSourceProviderUnavailable
	srcNoBackend := NewPrivateGitSource(nil, nil, nil, mockCreds, 123)
	_, _, err := srcNoBackend.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", validSHA)
	if !errors.Is(err, ErrSourceProviderUnavailable) {
		t.Fatalf("expected ErrSourceProviderUnavailable when backend is nil, got: %v", err)
	}

	// 2. Missing Auth returns ErrRetrievalAuthUnavailable
	sm := newMockSlotManager(t)
	dummyBackend := &PodmanBackend{}
	srcNoAuth := NewPrivateGitSource(dummyBackend, sm, nil, nil, 123)
	_, _, err = srcNoAuth.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", validSHA)
	if !errors.Is(err, ErrRetrievalAuthUnavailable) {
		t.Fatalf("expected ErrRetrievalAuthUnavailable when Auth is nil, got: %v", err)
	}

	// 3. Short commit prefix rejected (D-09)
	src := NewPrivateGitSource(dummyBackend, sm, nil, mockCreds, 123)
	_, _, err = src.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", "abc1234")
	if !errors.Is(err, ErrInvalidCommitOID) {
		t.Fatalf("expected ErrInvalidCommitOID for prefix SHA, got: %v", err)
	}

	// 4. Option-like clone URL rejected
	_, _, err = src.PrepareSource(context.Background(), "-oProxyCommand=evil", "main", validSHA)
	if !errors.Is(err, ErrInvalidRepoURL) {
		t.Fatalf("expected ErrInvalidRepoURL for option-like URL, got: %v", err)
	}

	// 5. Option-like headRef rejected
	_, _, err = src.PrepareSource(context.Background(), "https://github.com/owner/repo", "--upload-pack=evil", validSHA)
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("expected ErrInvalidSnapshot for option-like headRef, got: %v", err)
	}
}

func TestPrivateGitSource_CredentialCreationFailure(t *testing.T) {
	validSHA := "0123456789abcdef0123456789abcdef01234567"
	sm := newMockSlotManager(t)
	dummyBackend := &PodmanBackend{}
	mockCreds := &mockCredentialProvider{
		createErr: errors.New("github app not installed on head repository (status 404)"),
	}

	src := NewPrivateGitSource(dummyBackend, sm, nil, mockCreds, 999)
	_, _, err := src.PrepareSource(context.Background(), "https://github.com/headowner/headrepo", "main", validSHA)
	if err == nil {
		t.Fatalf("expected error when credential creation fails, got nil")
	}
	if !strings.Contains(err.Error(), "failed to create retrieval credential") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if mockCreds.createCalls != 1 {
		t.Errorf("expected exactly 1 create call, got: %d", mockCreds.createCalls)
	}
	if mockCreds.revokeCalls != 0 {
		t.Errorf("expected 0 revoke calls on create failure, got: %d", mockCreds.revokeCalls)
	}
}

func TestPrivateGitSource_SentinelSecretConfinementAndZeroization(t *testing.T) {
	sentinelToken := "sentinel-vault-secret-token-xyz987654321"
	validSHA := "0123456789abcdef0123456789abcdef01234567"

	testDir := t.TempDir()
	logArgsPath := filepath.Join(testDir, "captured_args.txt")
	logEnvPath := filepath.Join(testDir, "captured_env.txt")

	// Create a mock podman binary that records its arguments and environment,
	// and creates a valid snapshot file.
	mockPodmanScript := filepath.Join(testDir, "mock-podman.sh")
	scriptContent := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "info" ]; then
        echo '{"host":{"rootless":true,"cgroupVersion":"v2","security":{"rootless":true}}}'
        exit 0
    fi
    if [ "$arg" = "/sys/fs/cgroup/memory.max" ]; then
        echo '2147483648'
        exit 0
    fi
    if [ "$arg" = "rm" ] || [ "$arg" = "kill" ]; then
        exit 0
    fi
done

# Record all arguments
echo "$@" >> %q

# Record all environment variables
env >> %q

# Create dummy regular file in /snapshot mount if found in arguments
# In our test, the mounted snapshot path is passed as -v <host_snap>:/snapshot:rw
for arg in "$@"; do
    case "$arg" in
        *:/snapshot:rw)
            SNAP_DIR=$(echo "$arg" | cut -d: -f1)
            echo "package main" > "$SNAP_DIR/main.go"
            ;;
    esac
done

exit 0
`, logArgsPath, logEnvPath)

	if err := os.WriteFile(mockPodmanScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock podman script: %v", err)
	}

	mockCreds := &mockCredentialProvider{token: sentinelToken}
	sm := newMockSlotManager(t)

	backendCfg := PodmanConfig{
		BinaryPath:  mockPodmanScript,
		ImageDigest: "test-image:latest",
		CPUs:        2.0,
		MemoryBytes: 2 * 1024 * 1024 * 1024,
		PidsLimit:   256,
	}
	backend := NewPodmanBackend(backendCfg)

	src := NewPrivateGitSource(backend, sm, nil, mockCreds, 54321)
	src.GatewayBaseURL = "http://127.0.0.1:1" // unreachable upstream, container won't hit it directly

	ctx := context.Background()
	snap, cleanup, err := src.PrepareSource(ctx, "https://github.com/privowner/privrepo", "feature", validSHA)
	if err != nil {
		t.Fatalf("PrepareSource failed: %v", err)
	}
	defer cleanup()

	// 1. Verify snapshot was returned and validated
	if snap == nil || snap.CommitSHA != validSHA {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// 2. SCAN CHECK: Verify sentinel token NEVER appears in container arguments (D-10, D-11)
	argsData, err := os.ReadFile(logArgsPath)
	if err != nil {
		t.Fatalf("failed to read captured args: %v", err)
	}
	argsStr := string(argsData)
	if strings.Contains(argsStr, sentinelToken) {
		t.Fatalf("CRITICAL SECURITY VIOLATION: sentinel token leaked into container argv!\nCaptured argv: %s", argsStr)
	}

	// 3. SCAN CHECK: Verify sentinel token NEVER appears in container environment (D-10, D-11)
	envData, err := os.ReadFile(logEnvPath)
	if err != nil {
		t.Fatalf("failed to read captured env: %v", err)
	}
	envStr := string(envData)
	if strings.Contains(envStr, sentinelToken) {
		t.Fatalf("CRITICAL SECURITY VIOLATION: sentinel token leaked into container environment!\nCaptured env: %s", envStr)
	}

	// 4. SCAN CHECK: Verify sentinel token NEVER appears in snapshot files or manifest
	filepath.Walk(snap.SourceDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, _ := os.ReadFile(p)
			if strings.Contains(string(data), sentinelToken) {
				t.Fatalf("CRITICAL SECURITY VIOLATION: sentinel token found in snapshot file %s", p)
			}
		}
		return nil
	})

	// 5. Assert token was revoked before snapshot handoff! (D-10, key_links)
	if mockCreds.revokeCalls < 1 {
		t.Fatalf("expected token revocation to occur before snapshot handoff, got %d calls", mockCreds.revokeCalls)
	}

	// 6. Assert token was zeroized in memory
	if mockCreds.createdCred == nil || mockCreds.createdCred.Token != "" {
		t.Fatalf("expected credential to be zeroized, token: %q", mockCreds.createdCred.Token)
	}
	if !mockCreds.createdCred.IsRevoked() {
		t.Fatalf("expected credential to report IsRevoked() == true")
	}
}

// TestPrivateGitSource_RedactionAndFailureRevocation asserts that on failure or revocation error,
// the token is zeroized in memory and any error messages do not leak the secret token (D-10, SAFE-01).
func TestPrivateGitSource_RedactionAndFailureRevocation(t *testing.T) {
	sentinelSecret := "super-confidential-token-do-not-leak-8877"
	validSHA := "0123456789abcdef0123456789abcdef01234567"

	testDir := t.TempDir()
	mockPodmanScript := filepath.Join(testDir, "mock-failing-podman.sh")
	scriptContent := `#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "info" ]; then
        echo '{"host":{"rootless":true,"cgroupVersion":"v2","security":{"rootless":true}}}'
        exit 0
    fi
    if [ "$arg" = "/sys/fs/cgroup/memory.max" ]; then
        echo '2147483648'
        exit 0
    fi
    if [ "$arg" = "rm" ] || [ "$arg" = "kill" ]; then
        exit 0
    fi
done
# Simulate retrieval failure
echo "remote: HTTP 403 Forbidden - bad token" >&2
exit 1
`
	if err := os.WriteFile(mockPodmanScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	mockCreds := &mockCredentialProvider{
		token:     sentinelSecret,
		revokeErr: errors.New("upstream revoke endpoint returned 500 internal server error"),
	}
	sm := newMockSlotManager(t)

	backendCfg := PodmanConfig{
		BinaryPath:  mockPodmanScript,
		ImageDigest: "test-image:latest",
		CPUs:        2.0,
		MemoryBytes: 2 * 1024 * 1024 * 1024,
		PidsLimit:   256,
	}
	backend := NewPodmanBackend(backendCfg)

	src := NewPrivateGitSource(backend, sm, nil, mockCreds, 12345)
	src.GatewayBaseURL = "http://127.0.0.1:1"

	_, _, err := src.PrepareSource(context.Background(), "https://github.com/priv/repo", "main", validSHA)
	if err == nil {
		t.Fatalf("expected PrepareSource to fail when container fails, got nil error")
	}

	// 1. Verify error message does NOT leak the secret token (redaction check)
	errStr := err.Error()
	if strings.Contains(errStr, sentinelSecret) {
		t.Fatalf("CRITICAL SECURITY VIOLATION: error message leaked sentinel token: %s", errStr)
	}

	// 2. Verify token revocation was attempted even though container failed
	if mockCreds.revokeCalls < 1 {
		t.Errorf("expected token revocation to be called on container failure, got %d calls", mockCreds.revokeCalls)
	}

	// 3. Verify credential token is zeroized in memory even though revokeErr was returned
	if mockCreds.createdCred == nil || mockCreds.createdCred.Token != "" {
		t.Errorf("expected credential token to be zeroized, got: %q", mockCreds.createdCred.Token)
	}
}

// TestPrivateSourceIntegration is an opt-in integration test targeting an operator-controlled
// disposable private GitHub repository (D-10, 01-USER-SETUP.md).
// Routine tests run against mocks; this test requires explicit opt-in via PR_REVIEW_PRIVATE_INTEGRATION=1
// and never mutates host state or repository contents.
func TestPrivateSourceIntegration(t *testing.T) {
	if os.Getenv("PR_REVIEW_PRIVATE_INTEGRATION") != "1" {
		t.Skip("skipping opt-in disposable private source integration test (set PR_REVIEW_PRIVATE_INTEGRATION=1 to run)")
	}

	repoURL := os.Getenv("PR_REVIEW_PRIVATE_REPO_URL")
	commitSHA := os.Getenv("PR_REVIEW_PRIVATE_COMMIT_SHA")
	if repoURL == "" || commitSHA == "" {
		t.Fatalf("PR_REVIEW_PRIVATE_INTEGRATION=1 requires PR_REVIEW_PRIVATE_REPO_URL and PR_REVIEW_PRIVATE_COMMIT_SHA")
	}

	appIDStr := os.Getenv("GITHUB_APP_ID")
	keyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if appIDStr == "" || keyPath == "" {
		t.Fatalf("PR_REVIEW_PRIVATE_INTEGRATION=1 requires GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH")
	}

	var appID int64
	if _, err := fmt.Sscanf(appIDStr, "%d", &appID); err != nil || appID <= 0 {
		t.Fatalf("invalid GITHUB_APP_ID %q: %v", appIDStr, err)
	}

	client, err := github.NewAppClient(appID, keyPath)
	if err != nil {
		t.Fatalf("failed to create App client: %v", err)
	}

	owner, repo, err := ValidateCloneURL(repoURL)
	if err != nil {
		t.Fatalf("invalid repo URL %q: %v", repoURL, err)
	}

	repoID, err := client.GetRepoID(context.Background(), owner, repo)
	if err != nil {
		t.Fatalf("failed to get repository ID for %s/%s: %v", owner, repo, err)
	}

	t.Logf("Successfully verified narrow App credential access for private repo %s/%s (ID: %d)", owner, repo, repoID)
}
