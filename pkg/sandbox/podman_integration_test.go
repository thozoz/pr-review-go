package sandbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// TestPodmanTracer verifies real Podman rootless isolation against the deployed runtime.
// It is opt-in via PR_REVIEW_SANDBOX_INTEGRATION=1.
// When enabled, it verifies that missing prerequisites fail closed, and if operator-provisioned
// storage slots and images are available, executes live container verification probes.
func TestPodmanTracer(t *testing.T) {
	if os.Getenv("PR_REVIEW_SANDBOX_INTEGRATION") != "1" {
		t.Skip("skipping live deployment tracer: set PR_REVIEW_SANDBOX_INTEGRATION=1 to enable")
	}

	// 1. Verify fail-closed behavior for missing/invalid protections
	t.Run("PrerequisiteEnforcement", func(t *testing.T) {
		// Verify missing image digest fails closed
		backendNoImage := NewPodmanBackend(PodmanConfig{})
		if err := backendNoImage.Attest(context.Background()); err == nil {
			t.Fatalf("expected Attest to fail when ImageDigest is missing")
		}

		// Verify unmounted normal directory fails slot mount validation
		unmountedSlotDir := t.TempDir()
		controlDir := t.TempDir()
		slotMgr, err := NewLinuxSlotManager(unmountedSlotDir, controlDir)
		if err != nil {
			t.Fatalf("failed to create slot manager: %v", err)
		}
		if _, err := slotMgr.ValidateSlotMount(unmountedSlotDir); err == nil {
			t.Fatalf("expected ValidateSlotMount to fail on normal unmounted directory")
		}

		// Verify quarantined slot refuses acquisition
		slotMgr.SkipMountChecks = true
		if err := slotMgr.QuarantineSlot(unmountedSlotDir, "test failure"); err != nil {
			t.Fatalf("failed to quarantine slot: %v", err)
		}
		if _, err := slotMgr.AcquireSlot(context.Background(), "job-fail"); err == nil || !strings.Contains(err.Error(), "quarantined") {
			t.Fatalf("expected AcquireSlot to fail on quarantined slot, got: %v", err)
		}

		// Verify invalid/prefix commit SHAs fail closed
		invalidSnap := &Snapshot{CommitSHA: "abc1234", SourceDir: t.TempDir()}
		if err := invalidSnap.Validate(); err == nil {
			t.Fatalf("expected short prefix commit SHA to fail validation")
		}

		// Verify escaping symlinks fail closed
		outside := t.TempDir()
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		escapingSnap := &Snapshot{
			CommitSHA: "0123456789abcdef0123456789abcdef01234567",
			SourceDir: source,
		}
		if err := escapingSnap.Validate(); err == nil {
			t.Fatalf("expected escaping symlink to fail validation")
		}
	})

	// 2. Live execution against operator-provisioned environment
	t.Run("LiveExecution", func(t *testing.T) {
		podmanPath, err := exec.LookPath("podman")
		if err != nil {
			t.Logf("Podman binary not found in PATH: %v", err)
			return
		}

		imageDigest := os.Getenv("PR_REVIEW_SANDBOX_IMAGE")
		slotDir := os.Getenv("PR_REVIEW_SLOT_DIR")
		if imageDigest == "" || slotDir == "" {
			t.Logf("Operator setup required: set PR_REVIEW_SANDBOX_IMAGE and PR_REVIEW_SLOT_DIR to run live container probes")
			t.Logf("See 01-USER-SETUP.md for administrator and operator provisioning instructions")
			return
		}

		controlDir := os.Getenv("PR_REVIEW_CONTROL_DIR")
		if controlDir == "" {
			controlDir = t.TempDir()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		slotMgr, err := NewLinuxSlotManager(slotDir, controlDir)
		if err != nil {
			t.Fatalf("failed to initialize slot manager: %v", err)
		}

		mountID, err := slotMgr.ValidateSlotMount(slotDir)
		if err != nil {
			t.Fatalf("operator slot %q failed validation: %v", slotDir, err)
		}
		t.Logf("Operator slot verified: %q (mount ID %d)", slotDir, mountID)

		podmanCfg := PodmanConfig{
			BinaryPath:   podmanPath,
			ImageDigest:  imageDigest,
			CPUs:         2.0,
			MemoryBytes:  2147483648,
			PidsLimit:    256,
			TimeoutStage: 5 * time.Minute,
			TimeoutClean: 15 * time.Second,
		}
		backend := NewPodmanBackend(podmanCfg)

		if err := backend.Attest(ctx); err != nil {
			t.Fatalf("podman capability attestation failed: %v", err)
		}
		t.Logf("Podman capability attestation PASSED")

		// Prepare benign Go fixture
		fixtureDir := t.TempDir()
		goMod := "module tracer.test/fixture\n\ngo 1.22\n"
		if err := os.WriteFile(filepath.Join(fixtureDir, "go.mod"), []byte(goMod), 0644); err != nil {
			t.Fatal(err)
		}
		code := "package fixture\n\nfunc Add(a, b int) int { return a + b }\n"
		if err := os.WriteFile(filepath.Join(fixtureDir, "fixture.go"), []byte(code), 0644); err != nil {
			t.Fatal(err)
		}
		testCode := `package fixture

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2, 3) != 5")
	}
}

func TestProbe_NetworkIsolation(t *testing.T) {
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	_, err := d.Dial("tcp", "1.1.1.1:80")
	if err == nil {
		t.Fatalf("security violation: network connection succeeded under --network=none")
	}
}

func TestProbe_SealedSourceReadOnly(t *testing.T) {
	err := os.WriteFile("/snapshot/hostile.txt", []byte("tamper"), 0644)
	if err == nil {
		t.Fatalf("security violation: /snapshot was writable")
	}
}

func TestProbe_WorkDirWritable(t *testing.T) {
	f, err := os.CreateTemp("/work", "test-work-*")
	if err != nil {
		t.Fatalf("expected /work to be writable: %v", err)
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
}
`
		if err := os.WriteFile(filepath.Join(fixtureDir, "fixture_test.go"), []byte(testCode), 0644); err != nil {
			t.Fatal(err)
		}

		runnerCfg := &config.Config{
			SandboxCPUs:             2.0,
			SandboxMemoryBytes:      2147483648,
			SandboxPidsLimit:        256,
			SandboxDiskBytes:        MaxSlotCapacityBytes,
			SandboxTimeoutExecution: 5 * time.Minute,
			SandboxTimeoutCleanup:   15 * time.Second,
		}
		runner := NewRunnerWithConfig(runnerCfg, backend, slotMgr)

		sha := computeDirSHA(fixtureDir)
		snapshot := &Snapshot{
			CommitSHA: sha,
			SourceDir: fixtureDir,
		}

		report, err := runner.RunSnapshot(ctx, snapshot)
		if err != nil {
			t.Fatalf("RunSnapshot failed unexpectedly: %v", err)
		}
		if report.Status != StatusPassed {
			t.Fatalf("expected StatusPassed, got status=%q, reason=%q", report.Status, report.Reason)
		}
		t.Logf("Live snapshot execution PASSED: both build and race-test succeeded")

		// Verify slot cleaned up
		for _, sub := range []string{"work", "tmp", "cache", "snapshot"} {
			entries, err := os.ReadDir(filepath.Join(slotDir, sub))
			if err == nil && len(entries) > 0 {
				t.Fatalf("slot directory %q not cleaned up: %d items remain", sub, len(entries))
			}
		}
		t.Logf("Live slot cleanup verified")
	})
}

// TestPodmanSourceTracer verifies public exact-commit source retrieval, gateway restrictions,
// and tree export isolation against the deployed runtime (D-05, D-09, D-11, D-13, D-14, D-15).
// It is opt-in via PR_REVIEW_SANDBOX_INTEGRATION=1.
func TestPodmanSourceTracer(t *testing.T) {
	if os.Getenv("PR_REVIEW_SANDBOX_INTEGRATION") != "1" {
		t.Skip("skipping live deployment source tracer: set PR_REVIEW_SANDBOX_INTEGRATION=1 to enable")
	}

	// 1. Prerequisite and validation enforcement (fail-closed behavior)
	t.Run("PrerequisiteAndValidationEnforcement", func(t *testing.T) {
		// Verify prefix commit SHA fails closed
		if err := ValidateCommitOID("abc1234"); !errors.Is(err, ErrInvalidCommitOID) {
			t.Fatalf("expected ErrInvalidCommitOID for prefix commit SHA, got: %v", err)
		}

		// Verify non-https or option-like clone URL fails closed
		if _, _, err := ValidateCloneURL("http://github.com/foo/bar"); err == nil {
			t.Fatalf("expected ValidateCloneURL to reject non-https URL")
		}
		if _, _, err := ValidateCloneURL("-oProxyCommand=calc"); err == nil {
			t.Fatalf("expected ValidateCloneURL to reject option injection URL")
		}

		// Verify escaping symlink target fails closed
		if err := ValidateSymlinkTarget("entry.txt", "../outside"); err == nil {
			t.Fatalf("expected ValidateSymlinkTarget to reject escaping target")
		}

		// Verify traversal and Windows-unsafe path entries fail closed
		if err := ValidateSnapshotPath("../outside.txt"); err == nil {
			t.Fatalf("expected ValidateSnapshotPath to reject traversal path")
		}
		if err := ValidateSnapshotPath("aux.go"); !errors.Is(err, ErrUnsafePathEntry) {
			t.Fatalf("expected ValidateSnapshotPath to reject aux.go, got: %v", err)
		}
		if err := ValidateSnapshotPath("CON"); !errors.Is(err, ErrUnsafePathEntry) {
			t.Fatalf("expected ValidateSnapshotPath to reject CON, got: %v", err)
		}

		// Verify missing backend in PublicGitSource fails closed
		src := NewPublicGitSource(nil, nil, nil)
		validSHA := "0123456789abcdef0123456789abcdef01234567"
		if _, _, err := src.PrepareSource(context.Background(), "https://github.com/owner/repo", "main", validSHA); !errors.Is(err, ErrSourceProviderUnavailable) {
			t.Fatalf("expected ErrSourceProviderUnavailable when backend is nil, got: %v", err)
		}
	})

	// 2. Gateway route restrictions, redirect rejection, and SSRF enforcement
	t.Run("GatewayRestrictionsAndIsolation", func(t *testing.T) {
		var upstreamAuthHeader string
		upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamAuthHeader = r.Header.Get("Authorization")
			if strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack" {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
				_, _ = w.Write([]byte("001e# service=git-upload-pack\n0000"))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/git-upload-pack") && r.Method == http.MethodPost {
				w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
				_, _ = w.Write([]byte("PACK..."))
				return
			}
			http.NotFound(w, r)
		}))
		defer upstreamServer.Close()

		sockDir := t.TempDir()
		sockPath := filepath.Join(sockDir, "gw.sock")
		gw, err := StartGitGateway(GitGatewayConfig{
			SocketPath:        sockPath,
			RepoOwner:         "testowner",
			RepoName:          "testrepo",
			UpstreamBaseURL:   upstreamServer.URL,
			AllowTestLoopback: true,
		})
		if err != nil {
			t.Fatalf("failed to start git gateway: %v", err)
		}
		defer gw.Close()

		client := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
			Timeout: 3 * time.Second,
		}

		// Probe 1: Allowed smart read route
		req, _ := http.NewRequest(http.MethodGet, "http://unix/testowner/testrepo/info/refs?service=git-upload-pack", nil)
		req.Header.Set("Authorization", "Bearer injected-credential")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("allowed route failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for git-upload-pack route, got: %d", resp.StatusCode)
		}
		if upstreamAuthHeader != "" {
			t.Fatalf("expected Authorization header to be stripped, got: %q", upstreamAuthHeader)
		}

		// Probe 2: Deny git-receive-pack
		reqRecv, _ := http.NewRequest(http.MethodGet, "http://unix/testowner/testrepo/info/refs?service=git-receive-pack", nil)
		respRecv, err := client.Do(reqRecv)
		if err != nil {
			t.Fatal(err)
		}
		respRecv.Body.Close()
		if respRecv.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for receive-pack, got: %d", respRecv.StatusCode)
		}

		// Probe 3: Deny other repository path
		reqOther, _ := http.NewRequest(http.MethodGet, "http://unix/other/other/info/refs?service=git-upload-pack", nil)
		respOther, err := client.Do(reqOther)
		if err != nil {
			t.Fatal(err)
		}
		respOther.Body.Close()
		if respOther.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for unauthorized repository, got: %d", respOther.StatusCode)
		}

		// Probe 4: Upstream redirects denied
		redirServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://evil.com/redirect", http.StatusFound)
		}))
		defer redirServer.Close()

		sockPathRedir := filepath.Join(sockDir, "gw-redir.sock")
		gwRedir, err := StartGitGateway(GitGatewayConfig{
			SocketPath:        sockPathRedir,
			RepoOwner:         "testowner",
			RepoName:          "testrepo",
			UpstreamBaseURL:   redirServer.URL,
			AllowTestLoopback: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer gwRedir.Close()

		clientRedir := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPathRedir)
				},
			},
			Timeout: 3 * time.Second,
		}
		reqRedir, _ := http.NewRequest(http.MethodGet, "http://unix/testowner/testrepo/info/refs?service=git-upload-pack", nil)
		respRedir, err := clientRedir.Do(reqRedir)
		if err != nil {
			t.Fatal(err)
		}
		respRedir.Body.Close()
		if respRedir.StatusCode != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway when upstream redirects, got: %d", respRedir.StatusCode)
		}
	})

	// 3. Exact-commit tree export and hostile-content probes
	t.Run("ExactCommitSourceExportAndHostileProbes", func(t *testing.T) {
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Fatalf("git binary not found in PATH: %v", err)
		}

		fixtureDir := t.TempDir()
		runGit := func(args ...string) string {
			cmd := exec.Command(gitPath, args...)
			cmd.Dir = fixtureDir
			cmd.Env = []string{
				"GIT_AUTHOR_NAME=Tracer", "GIT_AUTHOR_EMAIL=tracer@example.com",
				"GIT_COMMITTER_NAME=Tracer", "GIT_COMMITTER_EMAIL=tracer@example.com",
				"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/tmp",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v failed: %v (output: %s)", args, err, string(out))
			}
			return strings.TrimSpace(string(out))
		}

		runGit("init")
		runGit("config", "user.name", "Tracer")
		runGit("config", "user.email", "tracer@example.com")

		// Create files: go.mod, code, instructions
		if err := os.WriteFile(filepath.Join(fixtureDir, "go.mod"), []byte("module tracer.test/source\n\ngo 1.22\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(fixtureDir, ".github"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, ".github", "copilot-instructions.md"), []byte("Use clean code conventions."), 0644); err != nil {
			t.Fatal(err)
		}

		runGit("add", ".")
		runGit("commit", "-m", "initial pinned source commit")
		cleanCommitSHA := runGit("rev-parse", "HEAD")

		// Probe 1: Exact tree export matches full commit OID
		destDir := t.TempDir()
		snap, err := ExportGitTree(context.Background(), fixtureDir, cleanCommitSHA, destDir)
		if err != nil {
			t.Fatalf("ExportGitTree failed for clean fixture: %v", err)
		}
		if snap.CommitSHA != cleanCommitSHA {
			t.Fatalf("expected commit %s, got %s", cleanCommitSHA, snap.CommitSHA)
		}
		if _, ok := snap.Manifest["main.go"]; !ok {
			t.Fatalf("main.go missing from manifest")
		}
		if _, err := os.Stat(filepath.Join(destDir, ".git")); !os.IsNotExist(err) {
			t.Fatalf(".git directory must be omitted from snapshot")
		}

		// Probe 2: Full commit OID mismatch rejected
		wrongSHA := "0123456789abcdef0123456789abcdef01234567"
		if _, err := ExportGitTree(context.Background(), fixtureDir, wrongSHA, t.TempDir()); err == nil {
			t.Fatalf("expected ExportGitTree to fail on mismatched commit SHA")
		}

		// Probe 3: Escaping symlink rejected
		_ = os.Symlink("../../outside", filepath.Join(fixtureDir, "escaping_link"))
		runGit("add", "escaping_link")
		runGit("commit", "-m", "add escaping link")
		linkSHA := runGit("rev-parse", "HEAD")
		if _, err := ExportGitTree(context.Background(), fixtureDir, linkSHA, t.TempDir()); err == nil {
			t.Fatalf("expected ExportGitTree to reject escaping symlink")
		}

		// Probe 4: Git LFS pointer detected and reported incomplete
		lfsContent := "version https://git-lfs.github.com/spec/v1\noid sha256:1111111111111111111111111111111111111111111111111111111111111111\nsize 42\n"
		if err := os.WriteFile(filepath.Join(fixtureDir, "asset.bin"), []byte(lfsContent), 0644); err != nil {
			t.Fatal(err)
		}
		runGit("add", "asset.bin")
		runGit("commit", "-m", "add lfs pointer")
		lfsSHA := runGit("rev-parse", "HEAD")
		lfsSnap, err := ExportGitTree(context.Background(), fixtureDir, lfsSHA, t.TempDir())
		if !errors.Is(err, ErrIncompleteGitLFS) {
			t.Fatalf("expected ErrIncompleteGitLFS, got: %v", err)
		}
		if lfsSnap == nil || !lfsSnap.IsIncomplete {
			t.Fatalf("expected snapshot to be marked IsIncomplete for Git LFS")
		}

		// Probe 5: Custom rule reads are confined and bounded
		runner := NewRunner(0)
		report := &VerificationReport{}
		runner.extractCustomRules(destDir, report)
		if report.RulesSource != ".github/copilot-instructions.md" {
			t.Fatalf("expected rules source .github/copilot-instructions.md, got %q", report.RulesSource)
		}
		if !strings.Contains(report.CustomRules, "Use clean code conventions.") {
			t.Fatalf("expected custom rules content to be extracted")
		}
	})

	// 4. Live execution against operator-provisioned environment (if configured)
	t.Run("LiveExecution", func(t *testing.T) {
		podmanPath, err := exec.LookPath("podman")
		if err != nil {
			t.Logf("Podman binary not found in PATH: %v", err)
			return
		}

		imageDigest := os.Getenv("PR_REVIEW_SANDBOX_IMAGE")
		slotDir := os.Getenv("PR_REVIEW_SLOT_DIR")
		if imageDigest == "" || slotDir == "" {
			t.Logf("Operator setup required: set PR_REVIEW_SANDBOX_IMAGE and PR_REVIEW_SLOT_DIR to run live container probes")
			t.Logf("See 01-USER-SETUP.md for administrator and operator provisioning instructions")
			return
		}

		controlDir := os.Getenv("PR_REVIEW_CONTROL_DIR")
		if controlDir == "" {
			controlDir = t.TempDir()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		slotMgr, err := NewLinuxSlotManager(slotDir, controlDir)
		if err != nil {
			t.Fatalf("failed to initialize slot manager: %v", err)
		}

		mountID, err := slotMgr.ValidateSlotMount(slotDir)
		if err != nil {
			t.Fatalf("operator slot %q failed validation: %v", slotDir, err)
		}
		t.Logf("Operator slot verified: %q (mount ID %d)", slotDir, mountID)

		podmanCfg := PodmanConfig{
			BinaryPath:   podmanPath,
			ImageDigest:  imageDigest,
			CPUs:         2.0,
			MemoryBytes:  2147483648,
			PidsLimit:    256,
			TimeoutStage: 5 * time.Minute,
			TimeoutClean: 15 * time.Second,
		}
		backend := NewPodmanBackend(podmanCfg)

		if err := backend.Attest(ctx); err != nil {
			t.Fatalf("podman capability attestation failed: %v", err)
		}
		t.Logf("Live source tracer attestation PASSED")
	})
}

