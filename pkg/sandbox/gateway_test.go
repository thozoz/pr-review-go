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

// TestGitGateway_RetrievalTokenInjectionAndIsolation verifies that the gateway strictly owns
// the App retrieval token, injects it only on approved canonical read routes, denies all
// out-of-repo or receive-pack routes, and securely erases the token upon closure (D-10, SAFE-01).
func TestGitGateway_RetrievalTokenInjectionAndIsolation(t *testing.T) {
	sentinelToken := "sentinel-app-retrieval-token-12345"
	var (
		receivedAuthHeader string
		upstreamCallCount  int
	)

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCallCount++
		receivedAuthHeader = r.Header.Get("Authorization")

		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = w.Write([]byte("001e# service=git-upload-pack\n0000"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = w.Write([]byte("PACK..."))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstreamServer.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "git-priv.sock")

	var onCloseCalled bool
	gw, err := StartGitGateway(GitGatewayConfig{
		SocketPath:        sockPath,
		RepoOwner:         "canonowner",
		RepoName:          "canonrepo",
		UpstreamBaseURL:   upstreamServer.URL,
		AllowTestLoopback: true,
		RetrievalToken:    sentinelToken,
		OnClose: func() {
			onCloseCalled = true
		},
	})
	if err != nil {
		t.Fatalf("failed to start git gateway: %v", err)
	}

	client := newUnixClient(sockPath)

	// 1. Authorized read route injects token and strips client-supplied auth header
	req1, _ := http.NewRequest(http.MethodGet, "http://unix/canonowner/canonrepo/info/refs?service=git-upload-pack", nil)
	req1.Header.Set("Authorization", "Bearer attacker-spoofed-token")
	req1.Header.Set("Proxy-Authorization", "Basic evil")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("request 1 failed: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got: %d", resp1.StatusCode)
	}
	if receivedAuthHeader != "Bearer "+sentinelToken {
		t.Errorf("expected gateway to inject sentinel token, got upstream header: %q", receivedAuthHeader)
	}

	// 2. Out-of-repo route must be denied with 403 Forbidden without contacting upstream
	callsBefore := upstreamCallCount
	req2, _ := http.NewRequest(http.MethodGet, "http://unix/otherowner/otherrepo/info/refs?service=git-upload-pack", nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("request 2 failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for out-of-repo request, got: %d", resp2.StatusCode)
	}
	if upstreamCallCount != callsBefore {
		t.Errorf("upstream was contacted for out-of-repo request!")
	}

	// 3. Receive-pack (write/push mutation) must be denied with 403 Forbidden
	req3, _ := http.NewRequest(http.MethodPost, "http://unix/canonowner/canonrepo/git-receive-pack", strings.NewReader("evil-pack"))
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("request 3 failed: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for git-receive-pack, got: %d", resp3.StatusCode)
	}
	if upstreamCallCount != callsBefore {
		t.Errorf("upstream was contacted for git-receive-pack request!")
	}

	// 4. Verify Close erases token and invokes OnClose
	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gateway: %v", err)
	}
	if !onCloseCalled {
		t.Errorf("expected OnClose hook to be invoked on gateway Close")
	}
	if gw.retrievalToken != "" {
		t.Errorf("expected retrievalToken to be erased after Close, got: %q", gw.retrievalToken)
	}
}

func TestDependencyGateway_AllowedGoModuleRoutes(t *testing.T) {
	var receivedAuthHeader string

	mockProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/golang.org/x/sync/@v/v0.7.0.mod":
			_, _ = w.Write([]byte("module golang.org/x/sync\n"))
		case "/golang.org/x/sync/@v/v0.7.0.info":
			_, _ = w.Write([]byte(`{"Version":"v0.7.0"}`))
		case "/golang.org/x/sync/@v/v0.7.0.zip":
			_, _ = w.Write([]byte("PK\x03\x04...mock-zip-bytes..."))
		case "/golang.org/x/sync/@v/list":
			_, _ = w.Write([]byte("v0.6.0\nv0.7.0\n"))
		case "/golang.org/x/sync/@latest":
			_, _ = w.Write([]byte(`{"Version":"v0.7.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockProxy.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		ProxyBaseURL:      mockProxy.URL,
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start dependency gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	testRoutes := []struct {
		path     string
		expected string
	}{
		{"/golang.org/x/sync/@v/v0.7.0.mod", "module golang.org/x/sync"},
		{"/golang.org/x/sync/@v/v0.7.0.info", `"Version":"v0.7.0"`},
		{"/golang.org/x/sync/@v/v0.7.0.zip", "PK\x03\x04...mock-zip-bytes..."},
		{"/golang.org/x/sync/@v/list", "v0.7.0"},
		{"/golang.org/x/sync/@latest", `"Version":"v0.7.0"`},
	}

	for _, tc := range testRoutes {
		req, _ := http.NewRequest(http.MethodGet, "http://unix"+tc.path, nil)
		req.Header.Set("Authorization", "Bearer attacker-spoofed")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("failed request to %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("route %s returned status %d, expected 200", tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), tc.expected) {
			t.Errorf("route %s returned unexpected body %q, expected containing %q", tc.path, string(body), tc.expected)
		}
		if receivedAuthHeader != "" {
			t.Errorf("route %s did not strip Authorization header, got: %q", tc.path, receivedAuthHeader)
		}
	}
}

func TestDependencyGateway_AllowedSumDBRoutes(t *testing.T) {
	mockSumDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/supported":
			_, _ = w.Write([]byte("ok"))
		case "/latest":
			_, _ = w.Write([]byte("go.sum database tree\n42\n"))
		case "/lookup/golang.org/x/sync@v0.7.0":
			_, _ = w.Write([]byte("golang.org/x/sync v0.7.0 h1:mockhash=\n"))
		case "/tile/8/0/x261/490":
			_, _ = w.Write([]byte("mock-tile-data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockSumDB.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		SumDBBaseURL:      mockSumDB.URL,
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start dependency gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	testRoutes := []struct {
		path     string
		expected string
	}{
		{"/sumdb/sum.golang.org/supported", "ok"},
		{"/sumdb/sum.golang.org/latest", "go.sum database tree"},
		{"/sumdb/sum.golang.org/lookup/golang.org/x/sync@v0.7.0", "mockhash="},
		{"/sumdb/sum.golang.org/tile/8/0/x261/490", "mock-tile-data"},
	}

	for _, tc := range testRoutes {
		req, _ := http.NewRequest(http.MethodGet, "http://unix"+tc.path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("failed request to %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("route %s returned status %d, expected 200", tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), tc.expected) {
			t.Errorf("route %s returned unexpected body %q, expected containing %q", tc.path, string(body), tc.expected)
		}
	}
}

func TestDependencyGateway_RedirectHandlingAndAllowlist(t *testing.T) {
	// Storage server serving downloaded blob
	storageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("GCS-STREAMED-ZIP-BYTES"))
	}))
	defer storageServer.Close()

	// Redirect server that can return clean redirect, redirect loop, or untrusted redirect
	var mode string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "clean":
			http.Redirect(w, r, storageServer.URL+"/pkg.zip", http.StatusFound)
		case "loop":
			http.Redirect(w, r, r.URL.String(), http.StatusFound)
		case "untrusted":
			http.Redirect(w, r, "http://evil-attacker.example.com/payload.zip", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxyServer.Close()

	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		ProxyBaseURL:      proxyServer.URL,
		AllowedHosts:      []string{storageServer.URL},
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	// 1. Clean redirect to allowed host is followed transparently and streamed
	mode = "clean"
	req1, _ := http.NewRequest(http.MethodGet, "http://unix/golang.org/x/sync/@v/v0.7.0.zip", nil)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("request 1 failed: %v", err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK after clean redirect, got: %d", resp1.StatusCode)
	}
	if string(body1) != "GCS-STREAMED-ZIP-BYTES" {
		t.Errorf("expected streamed zip body, got: %q", string(body1))
	}

	// 2. Redirect loop (>3 hops) is rejected with 502 Bad Gateway
	mode = "loop"
	req2, _ := http.NewRequest(http.MethodGet, "http://unix/golang.org/x/sync/@v/v0.7.0.zip", nil)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("request 2 failed: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway on redirect loop, got: %d", resp2.StatusCode)
	}

	// 3. Redirect to un-allowlisted destination is rejected with 403 Forbidden
	mode = "untrusted"
	req3, _ := http.NewRequest(http.MethodGet, "http://unix/golang.org/x/sync/@v/v0.7.0.zip", nil)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("request 3 failed: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden on untrusted redirect, got: %d", resp3.StatusCode)
	}
}

func TestDependencyGateway_DeniedMethodsAndCONNECT(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	deniedMethods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, m := range deniedMethods {
		req, _ := http.NewRequest(m, "http://unix/golang.org/x/sync/@v/list", strings.NewReader("payload"))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("method %s failed: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("expected 405 Method Not Allowed for %s, got: %d", m, resp.StatusCode)
		}
	}
}

func TestDependencyGateway_DeniedArbitraryRoutesAndTraversal(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		AllowTestLoopback: true,
	})
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)

	deniedPaths := []string{
		"/etc/passwd",
		"/api/v1/user",
		"/golang.org/x/sync/../../etc/passwd",
		"/golang.org/x/sync/%2e%2e/passwd",
		"/golang.org/x/sync//file",
		"/sumdb/other.org/latest",
	}

	for _, p := range deniedPaths {
		req, _ := http.NewRequest(http.MethodGet, "http://unix"+p, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("path %s failed: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for path %s, got: %d", p, resp.StatusCode)
		}
	}
}

func TestDependencyGateway_SSRFProtectionByDefault(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	// AllowTestLoopback = false enforces private IP denial
	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		ProxyBaseURL:      "http://127.0.0.1:9876",
		AllowTestLoopback: false,
	})
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}
	defer gw.Close()

	client := newUnixClient(sockPath)
	req, _ := http.NewRequest(http.MethodGet, "http://unix/golang.org/x/sync/@v/list", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway when SSRF triggers, got: %d", resp.StatusCode)
	}
}

func TestDependencyGateway_LifecycleAndSocketCleanup(t *testing.T) {
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "depgw.sock")

	var onCloseCalled bool
	gw, err := StartDependencyGateway(DependencyGatewayConfig{
		SocketPath:        sockPath,
		AllowTestLoopback: true,
		OnClose: func() {
			onCloseCalled = true
		},
	})
	if err != nil {
		t.Fatalf("failed to start gateway: %v", err)
	}

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket should exist: %v", err)
	}

	if err := gw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if !onCloseCalled {
		t.Errorf("OnClose hook was not invoked")
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file was not removed after Close")
	}
}

