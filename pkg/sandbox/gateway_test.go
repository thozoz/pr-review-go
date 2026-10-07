package sandbox

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newUnixClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 5 * time.Second,
	}
}

func TestGitGateway_AllowedRoutesAndForwarding(t *testing.T) {
	var receivedAuthHeader string
	var receivedPath string
	var receivedService string
	var receivedBody []byte

	// Mock upstream Git server
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedService = r.URL.Query().Get("service")
		receivedAuthHeader = r.Header.Get("Authorization")
		receivedBody, _ = io.ReadAll(r.Body)

		if strings.HasSuffix(r.URL.Path, "/info/refs") && receivedService == "git-upload-pack" {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = w.Write([]byte("001e# service=git-upload-pack\n00000048abc1234 HEAD\x00symref=HEAD:refs/heads/main\n0000"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = w.Write([]byte("PACK...mock-packfile-bytes..."))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstreamServer.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

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

	client := newUnixClient(sockPath)

	// 1. Test GET /testowner/testrepo/info/refs?service=git-upload-pack
	req, err := http.NewRequest(http.MethodGet, "http://unix/testowner/testrepo/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer attacker-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "service=git-upload-pack") {
		t.Fatalf("unexpected body: %s", string(body))
	}
	if !strings.Contains(receivedPath, "testrepo") {
		t.Fatalf("expected receivedPath to contain testrepo, got: %s", receivedPath)
	}
	// Verify Authorization header was stripped before forwarding
	if receivedAuthHeader != "" {
		t.Fatalf("expected Authorization header to be stripped, but upstream received: %q", receivedAuthHeader)
	}

	// 2. Test POST /testowner/testrepo/git-upload-pack
	postReq, err := http.NewRequest(http.MethodPost, "http://unix/testowner/testrepo/git-upload-pack", strings.NewReader("0014want abc1234\n0000"))
	if err != nil {
		t.Fatal(err)
	}
	postReq.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	postResp, err := client.Do(postReq)
	if err != nil {
		t.Fatalf("failed to make POST request: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for git-upload-pack, got: %d", postResp.StatusCode)
	}
	postBody, _ := io.ReadAll(postResp.Body)
	if !strings.Contains(string(postBody), "PACK...") {
		t.Fatalf("unexpected post body: %s", string(postBody))
	}
	if !bytes.Contains(receivedBody, []byte("want abc1234")) {
		t.Fatalf("upstream did not receive request body: %s", string(receivedBody))
	}
}

func TestGitGateway_DeniesReceivePackAndMutations(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "testowner",
		RepoName:          "testrepo",
		UpstreamBaseURL:   "http://example.com",
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start git gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	// 1. GET with service=git-receive-pack
	req, _ := http.NewRequest(http.MethodGet, "http://unix/testowner/testrepo/info/refs?service=git-receive-pack", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for receive-pack, got: %d", resp.StatusCode)
	}

	// 2. POST to /git-receive-pack
	postReq, _ := http.NewRequest(http.MethodPost, "http://unix/testowner/testrepo/git-receive-pack", strings.NewReader("payload"))
	postResp, err := client.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for /git-receive-pack, got: %d", postResp.StatusCode)
	}
}

func TestGitGateway_DeniesOtherRepositoriesAndTraversal(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "authorized-owner",
		RepoName:          "authorized-repo",
		UpstreamBaseURL:   "http://example.com",
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	// Requesting different repo
	req, _ := http.NewRequest(http.MethodGet, "http://unix/other-owner/other-repo/info/refs?service=git-upload-pack", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unauthorized repository, got: %d", resp.StatusCode)
	}

	// Traversal attempt
	reqTrav, _ := http.NewRequest(http.MethodGet, "http://unix/authorized-owner/authorized-repo/../../etc/passwd", nil)
	respTrav, err := client.Do(reqTrav)
	if err != nil {
		t.Fatal(err)
	}
	respTrav.Body.Close()
	if respTrav.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for path traversal, got: %d", respTrav.StatusCode)
	}
}

func TestGitGateway_DeniesUpstreamRedirects(t *testing.T) {
	// Upstream returns 302 redirect
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://attacker-controlled.site/pwn", http.StatusFound)
	}))
	defer redirectServer.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "owner",
		RepoName:          "repo",
		UpstreamBaseURL:   redirectServer.URL,
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)
	req, _ := http.NewRequest(http.MethodGet, "http://unix/owner/repo/info/refs?service=git-upload-pack", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway when upstream redirects, got: %d", resp.StatusCode)
	}
}

func TestGitGateway_DeniesSSRFByDefault(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

	// Upstream points to localhost without AllowTestLoopback
	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "owner",
		RepoName:          "repo",
		UpstreamBaseURL:   "http://127.0.0.1:9999",
		AllowTestLoopback: false, // Enforce SSRF protection
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)
	req, _ := http.NewRequest(http.MethodGet, "http://unix/owner/repo/info/refs?service=git-upload-pack", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway when SSRF triggers, got: %d", resp.StatusCode)
	}
}

func TestGitGateway_LifecycleAndSocketCleanup(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git.sock")

	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "owner",
		RepoName:          "repo",
		UpstreamBaseURL:   "https://github.com",
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file should exist: %v", err)
	}

	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gateway: %v", err)
	}

	// Socket file should be removed on close
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after Close(), got: %v", err)
	}
}
