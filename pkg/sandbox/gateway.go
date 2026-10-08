package sandbox

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

var (
	ErrGatewayClosed         = errors.New("git gateway is closed")
	ErrGatewaySSRFDenied     = errors.New("gateway access to private or local destination denied (SSRF protection)")
	ErrGatewayRouteDenied    = errors.New("gateway route denied: only approved repository git-upload-pack routes are permitted")
	ErrGatewayMethodDenied   = errors.New("gateway method denied: only GET and POST are permitted")
	ErrGatewayRedirectDenied  = errors.New("gateway upstream redirect denied: following redirects is prohibited")

	ErrDependencyRouteDenied  = errors.New("dependency gateway route denied: only approved Go module and sumdb protocol paths are permitted")
	ErrDependencyMethodDenied = errors.New("dependency gateway method denied: only GET and HEAD are permitted")
	ErrDependencyHostDenied   = errors.New("dependency gateway upstream host denied: destination not in allowlist")
	ErrDependencyRedirectLoop = errors.New("dependency gateway redirect loop or depth exceeded")
)

// GitGatewayConfig defines the configuration for the preparation-only Git data gateway.
type GitGatewayConfig struct {
	SocketPath        string        // Path to the Unix domain socket
	RepoOwner         string        // Authorized canonical repository owner (e.g. "thozoz")
	RepoName          string        // Authorized canonical repository name (e.g. "pr-review-go")
	UpstreamBaseURL   string        // Base URL for upstream Git service (default: "https://github.com")
	MaxBodyBytes      int64         // Maximum request/response body size (default: 100 MiB)
	RequestTimeout    time.Duration // Timeout for upstream requests (default: 2 minutes)
	AllowTestLoopback bool          // Allow localhost/loopback destinations ONLY in automated test fixtures
	RetrievalToken    string        // Narrow, short-lived App retrieval token injected strictly by the gateway (D-10, SAFE-01)
	OnClose           func()        // Optional cleanup hook invoked on Close (e.g. token revocation)
}

// GitGateway is a restricted Unix domain socket proxy that forwards ONLY git-upload-pack
// smart HTTP read routes for a specific canonical repository.
// It accepts no command or mount operations, prevents SSRF, and rejects redirects and receive-pack.
type GitGateway struct {
	cfg            GitGatewayConfig
	listener       net.Listener
	server         *http.Server
	client         *http.Client
	mu             sync.Mutex
	closed         bool
	retrievalToken string
}

// isRestrictedIP checks if an IP is a loopback, link-local, private, or multicast address.
func isRestrictedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// Check standard private address ranges
	if ip4 := ip.To4(); ip4 != nil {
		// 10.0.0.0/8
		if ip4[0] == 10 {
			return true
		}
		// 172.16.0.0/12
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		// 192.168.0.0/16
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}
		// 100.64.0.0/10 (Carrier-grade NAT)
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 169.254.0.0/16 (Link-local)
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	} else {
		// IPv6 Unique Local Address (fc00::/7)
		if len(ip) >= 2 && (ip[0]&0xfe) == 0xfc {
			return true
		}
	}
	return false
}

// StartGitGateway creates and starts a new restricted Git gateway listening on a Unix domain socket.
func StartGitGateway(cfg GitGatewayConfig) (*GitGateway, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("socket path cannot be empty")
	}
	if cfg.RepoOwner == "" || cfg.RepoName == "" {
		return nil, errors.New("repository owner and name must be specified")
	}
	if cfg.UpstreamBaseURL == "" {
		cfg.UpstreamBaseURL = "https://github.com"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 100 * 1024 * 1024 // 100 MiB
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}

	upstreamURL, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream base URL: %w", err)
	}

	// Remove any existing socket at the path
	_ = os.Remove(cfg.SocketPath)

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", cfg.SocketPath, err)
	}

	// Ensure container user can communicate over the socket
	_ = os.Chmod(cfg.SocketPath, 0666)

	// Custom Transport with SSRF protection and TLS validation
	transport := &http.Transport{
		Proxy: nil, // Do not inherit host proxy environment
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			// Resolve host to IP
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %s", host)
			}

			var dialIP net.IP
			for _, ip := range ips {
				if !cfg.AllowTestLoopback && isRestrictedIP(ip) {
					continue
				}
				dialIP = ip
				break
			}

			if dialIP == nil {
				return nil, fmt.Errorf("%w: all resolved IPs for %s are private or restricted (%v)", ErrGatewaySSRFDenied, host, ips)
			}

			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}

			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
			if err != nil {
				return nil, err
			}

			// Wrap in TLS if connecting to HTTPS upstream
			if upstreamURL.Scheme == "https" {
				tlsConn := tls.Client(conn, &tls.Config{
					ServerName: host,
					MinVersion: tls.VersionTLS12,
				})
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, fmt.Errorf("TLS handshake failed for %s: %w", host, err)
				}
				return tlsConn, nil
			}

			return conn, nil
		},
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}

	// Client that rejects redirects explicitly (D-05, SAFE-01)
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	gw := &GitGateway{
		cfg:            cfg,
		listener:       listener,
		client:         client,
		retrievalToken: cfg.RetrievalToken,
	}

	handler := http.HandlerFunc(gw.handleRequest)
	server := &http.Server{
		Handler:      handler,
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: cfg.RequestTimeout,
	}
	gw.server = server

	go func() {
		_ = server.Serve(listener)
	}()

	return gw, nil
}

// handleRequest enforces strict route, method, and protocol filtering.
func (g *GitGateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	// 1. Method check: only GET and POST permitted
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, ErrGatewayMethodDenied.Error(), http.StatusMethodNotAllowed)
		return
	}

	// 2. Reject CONNECT
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT method is prohibited", http.StatusMethodNotAllowed)
		return
	}

	// 3. Deny receive-pack explicitly (receive-pack is for git push/mutations)
	if strings.Contains(r.URL.Path, "git-receive-pack") || strings.Contains(r.URL.RawQuery, "git-receive-pack") {
		http.Error(w, "git-receive-pack is strictly prohibited: read-only retrieval only", http.StatusForbidden)
		return
	}

	// 4. Validate canonical repository path
	cleanPath := path.Clean(r.URL.Path)
	ownerClean := strings.ToLower(g.cfg.RepoOwner)
	repoClean := strings.ToLower(strings.TrimSuffix(g.cfg.RepoName, ".git"))

	expectedPrefixNoGit := fmt.Sprintf("/%s/%s", ownerClean, repoClean)
	expectedPrefixWithGit := fmt.Sprintf("/%s/%s.git", ownerClean, repoClean)

	pathLower := strings.ToLower(cleanPath)
	if !strings.HasPrefix(pathLower, expectedPrefixNoGit) && !strings.HasPrefix(pathLower, expectedPrefixWithGit) {
		http.Error(w, fmt.Sprintf("%s: path %s does not match repository %s/%s", ErrGatewayRouteDenied, cleanPath, g.cfg.RepoOwner, g.cfg.RepoName), http.StatusForbidden)
		return
	}

	// Ensure the sub-path is ONLY /info/refs or /git-upload-pack
	subPath := strings.TrimPrefix(pathLower, expectedPrefixWithGit)
	if subPath == pathLower {
		subPath = strings.TrimPrefix(pathLower, expectedPrefixNoGit)
	}

	switch subPath {
	case "/info/refs":
		if r.Method != http.MethodGet {
			http.Error(w, "only GET is permitted for /info/refs", http.StatusMethodNotAllowed)
			return
		}
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" {
			http.Error(w, fmt.Sprintf("unsupported service %q; only service=git-upload-pack is allowed", service), http.StatusForbidden)
			return
		}
	case "/git-upload-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "only POST is permitted for /git-upload-pack", http.StatusMethodNotAllowed)
			return
		}
	default:
		http.Error(w, fmt.Sprintf("%s: subpath %q is not permitted", ErrGatewayRouteDenied, subPath), http.StatusForbidden)
		return
	}

	// 5. Construct upstream request
	upstreamURL, err := url.Parse(g.cfg.UpstreamBaseURL)
	if err != nil {
		http.Error(w, "upstream configuration error", http.StatusInternalServerError)
		return
	}

	targetURL := &url.URL{
		Scheme:   upstreamURL.Scheme,
		Host:     upstreamURL.Host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}

	var bodyReader io.Reader
	if r.Body != nil {
		bodyReader = io.LimitReader(r.Body, g.cfg.MaxBodyBytes)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bodyReader)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create upstream request: %v", err), http.StatusInternalServerError)
		return
	}

	// Copy only approved Git client headers
	for _, h := range []string{"Accept", "Content-Type", "Git-Protocol", "User-Agent"} {
		if val := r.Header.Get(h); val != "" {
			req.Header.Set(h, val)
		}
	}

	// Explicitly strip any authorization headers supplied by the client
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")

	// Inject narrow retrieval token only on canonical same-repo read routes (D-10, SAFE-01)
	g.mu.Lock()
	token := g.retrievalToken
	g.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// 6. Forward to upstream
	resp, err := g.client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 7. Check for upstream redirects (3xx) - MUST NOT FOLLOW or pass through
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		http.Error(w, ErrGatewayRedirectDenied.Error(), http.StatusBadGateway)
		return
	}

	// 8. Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 9. Stream bounded response body
	limitedBody := io.LimitReader(resp.Body, g.cfg.MaxBodyBytes)
	_, _ = io.Copy(w, limitedBody)
}

// Close gracefully closes the gateway server, listener, and removes the Unix socket.
// It immediately erases any stored retrieval token from memory.
func (g *GitGateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		return nil
	}
	g.closed = true

	// Immediately erase retrieval token and state (D-10, D-11)
	g.retrievalToken = ""
	g.cfg.RetrievalToken = ""

	onClose := g.cfg.OnClose
	g.cfg.OnClose = nil
	if onClose != nil {
		onClose()
	}

	var err error
	if g.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = g.server.Shutdown(ctx)
	}
	if g.listener != nil {
		lErr := g.listener.Close()
		if lErr != nil && !errors.Is(lErr, net.ErrClosed) && !strings.Contains(lErr.Error(), "use of closed network connection") {
			err = lErr
		}
	}

	if g.cfg.SocketPath != "" {
		_ = os.Remove(g.cfg.SocketPath)
	}

	return err
}

// DependencyGatewayConfig defines the configuration for the restricted Go module data gateway (D-05, SAFE-01).
type DependencyGatewayConfig struct {
	SocketPath        string        // Path to the Unix domain socket
	ProxyBaseURL      string        // Base URL for Go module proxy (default: "https://proxy.golang.org")
	SumDBBaseURL      string        // Base URL for Checksum DB (default: "https://sum.golang.org")
	AllowedHosts      []string      // Allowed upstream hosts (default: proxy.golang.org, sum.golang.org, storage.googleapis.com)
	MaxBodyBytes      int64         // Maximum streamed response size (default: 500 MiB)
	RequestTimeout    time.Duration // Timeout for upstream requests (default: 2 minutes)
	AllowTestLoopback bool          // Allow localhost/loopback destinations ONLY in automated test fixtures
	OnClose           func()        // Optional cleanup hook invoked on Close
}

// DependencyGateway is a restricted Unix domain socket proxy that forwards ONLY Go module and sumdb
// protocol paths. It strictly limits destinations to allowlisted hosts, enforces SSRF and DNS
// protections, follows only allowlisted module-zip redirects, and streams bodies without host buffering.
type DependencyGateway struct {
	cfg      DependencyGatewayConfig
	listener net.Listener
	server   *http.Server
	client   *http.Client
	mu       sync.Mutex
	closed   bool
}

func isAllowedHost(target string, allowed []string) bool {
	h := strings.ToLower(target)
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	for _, a := range allowed {
		cleanA := strings.ToLower(a)
		if allowedHostOnly, _, err := net.SplitHostPort(cleanA); err == nil {
			if h == allowedHostOnly || strings.EqualFold(target, a) {
				return true
			}
		}
		if h == cleanA || strings.EqualFold(target, a) {
			return true
		}
	}
	return false
}

func isValidGoModuleProxyPath(p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	clean := path.Clean(p)
	if clean == "/" || clean == "." {
		return false
	}
	if strings.HasSuffix(clean, "/@v/list") {
		mod := strings.TrimSuffix(clean, "/@v/list")
		return len(strings.Trim(mod, "/")) > 0
	}
	if strings.HasSuffix(clean, "/@latest") {
		mod := strings.TrimSuffix(clean, "/@latest")
		return len(strings.Trim(mod, "/")) > 0
	}
	idx := strings.Index(clean, "/@v/")
	if idx > 0 {
		mod := clean[:idx]
		rest := clean[idx+len("/@v/"):]
		if len(strings.Trim(mod, "/")) == 0 || len(rest) == 0 {
			return false
		}
		if strings.HasSuffix(rest, ".info") || strings.HasSuffix(rest, ".mod") || strings.HasSuffix(rest, ".zip") {
			return true
		}
	}
	return false
}

func isValidSumDBPath(p string) bool {
	if strings.Contains(p, "..") {
		return false
	}
	clean := path.Clean(p)
	const prefix = "/sumdb/sum.golang.org"
	if !strings.HasPrefix(clean, prefix) {
		return false
	}
	sub := strings.TrimPrefix(clean, prefix)
	if sub == "" || sub == "/" {
		return false
	}
	if sub == "/supported" || sub == "/latest" {
		return true
	}
	if strings.HasPrefix(sub, "/lookup/") && len(sub) > len("/lookup/") {
		return true
	}
	if strings.HasPrefix(sub, "/tile/") && len(sub) > len("/tile/") {
		return true
	}
	return false
}

// StartDependencyGateway creates and starts a new restricted Go module dependency gateway.
func StartDependencyGateway(cfg DependencyGatewayConfig) (*DependencyGateway, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("socket path cannot be empty")
	}
	if cfg.ProxyBaseURL == "" {
		cfg.ProxyBaseURL = "https://proxy.golang.org"
	}
	if cfg.SumDBBaseURL == "" {
		cfg.SumDBBaseURL = "https://sum.golang.org"
	}
	if len(cfg.AllowedHosts) == 0 {
		cfg.AllowedHosts = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 500 * 1024 * 1024 // 500 MiB
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}

	proxyURL, err := url.Parse(cfg.ProxyBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy base URL: %w", err)
	}
	sumdbURL, err := url.Parse(cfg.SumDBBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid sumdb base URL: %w", err)
	}

	if proxyURL.Hostname() != "" && !isAllowedHost(proxyURL.Hostname(), cfg.AllowedHosts) {
		cfg.AllowedHosts = append(cfg.AllowedHosts, proxyURL.Hostname())
	}
	if sumdbURL.Hostname() != "" && !isAllowedHost(sumdbURL.Hostname(), cfg.AllowedHosts) {
		cfg.AllowedHosts = append(cfg.AllowedHosts, sumdbURL.Hostname())
	}

	_ = os.Remove(cfg.SocketPath)

	listener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", cfg.SocketPath, err)
	}
	_ = os.Chmod(cfg.SocketPath, 0666)

	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			if !isAllowedHost(host, cfg.AllowedHosts) && !isAllowedHost(addr, cfg.AllowedHosts) {
				return nil, fmt.Errorf("%w: host %s is not in allowed hosts %v", ErrDependencyHostDenied, host, cfg.AllowedHosts)
			}

			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("DNS resolution failed for %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP addresses resolved for %s", host)
			}

			var dialIP net.IP
			for _, ip := range ips {
				if !cfg.AllowTestLoopback && isRestrictedIP(ip) {
					continue
				}
				dialIP = ip
				break
			}
			if dialIP == nil {
				return nil, fmt.Errorf("%w: all resolved IPs for %s are private or restricted (%v)", ErrGatewaySSRFDenied, host, ips)
			}

			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	gw := &DependencyGateway{
		cfg:      cfg,
		listener: listener,
		client:   client,
	}

	server := &http.Server{
		Handler:      http.HandlerFunc(gw.handleRequest),
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: cfg.RequestTimeout,
	}
	gw.server = server

	go func() {
		_ = server.Serve(listener)
	}()

	return gw, nil
}

func (g *DependencyGateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	// 1. Method check: only GET and HEAD permitted
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, ErrDependencyMethodDenied.Error(), http.StatusMethodNotAllowed)
		return
	}

	// 2. Reject CONNECT
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT method is prohibited", http.StatusMethodNotAllowed)
		return
	}

	// 3. Reject userinfo, ports (unless AllowTestLoopback), and invalid characters
	if r.URL.User != nil {
		http.Error(w, "userinfo in URL is prohibited", http.StatusBadRequest)
		return
	}
	if !g.cfg.AllowTestLoopback && r.URL.Port() != "" {
		http.Error(w, "port in URL is prohibited", http.StatusBadRequest)
		return
	}

	// 4. Reject traversal or double-escaped paths
	rawURI := r.URL.RequestURI()
	rawPath := r.URL.Path
	if strings.Contains(rawURI, "..") || strings.Contains(rawPath, "..") ||
		strings.Contains(strings.ToLower(rawURI), "%2e%2e") ||
		strings.Contains(rawURI, "//") {
		http.Error(w, fmt.Sprintf("%s: traversal or malformed path", ErrDependencyRouteDenied), http.StatusForbidden)
		return
	}

	cleanPath := path.Clean(r.URL.Path)

	// 5. Route validation & upstream target selection
	var targetBaseURL string
	var targetPath string

	if strings.HasPrefix(cleanPath, "/sumdb/sum.golang.org") {
		if !isValidSumDBPath(cleanPath) {
			http.Error(w, fmt.Sprintf("%s: invalid sumdb path %q", ErrDependencyRouteDenied, cleanPath), http.StatusForbidden)
			return
		}
		sub := strings.TrimPrefix(cleanPath, "/sumdb/sum.golang.org")
		targetBaseURL = g.cfg.SumDBBaseURL
		targetPath = sub
	} else {
		if !isValidGoModuleProxyPath(cleanPath) {
			http.Error(w, fmt.Sprintf("%s: invalid module proxy path %q", ErrDependencyRouteDenied, cleanPath), http.StatusForbidden)
			return
		}
		targetBaseURL = g.cfg.ProxyBaseURL
		targetPath = cleanPath
	}

	baseParsed, err := url.Parse(targetBaseURL)
	if err != nil {
		http.Error(w, "upstream configuration error", http.StatusInternalServerError)
		return
	}

	targetURL := &url.URL{
		Scheme: baseParsed.Scheme,
		Host:   baseParsed.Host,
		Path:   targetPath,
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create upstream request: %v", err), http.StatusInternalServerError)
		return
	}

	// Copy approved headers only
	for _, h := range []string{"Accept", "User-Agent", "If-None-Match"} {
		if val := r.Header.Get(h); val != "" {
			req.Header.Set(h, val)
		}
	}
	// Explicitly strip client auth headers
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")

	// 6. Forward with bounded redirect handling (up to 3 hops)
	currentReq := req
	var resp *http.Response

	for hop := 0; hop <= 3; hop++ {
		resp, err = g.client.Do(currentReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
			return
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				http.Error(w, "upstream redirect missing Location header", http.StatusBadGateway)
				return
			}
			if hop == 3 {
				http.Error(w, ErrDependencyRedirectLoop.Error(), http.StatusBadGateway)
				return
			}

			locURL, err := url.Parse(loc)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid redirect URL: %v", err), http.StatusBadGateway)
				return
			}
			if !locURL.IsAbs() {
				locURL = currentReq.URL.ResolveReference(locURL)
			}

			if locURL.Scheme != "https" && !(g.cfg.AllowTestLoopback && locURL.Scheme == "http") {
				http.Error(w, fmt.Sprintf("redirect scheme %q is prohibited", locURL.Scheme), http.StatusForbidden)
				return
			}

			if !isAllowedHost(locURL.Hostname(), g.cfg.AllowedHosts) && !isAllowedHost(locURL.Host, g.cfg.AllowedHosts) {
				http.Error(w, fmt.Sprintf("%s: redirect host %q is not in allowlist", ErrDependencyHostDenied, locURL.Host), http.StatusForbidden)
				return
			}

			if locURL.User != nil {
				http.Error(w, "userinfo in redirect is prohibited", http.StatusForbidden)
				return
			}

			nextReq, err := http.NewRequestWithContext(r.Context(), r.Method, locURL.String(), nil)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed creating redirect request: %v", err), http.StatusInternalServerError)
				return
			}
			for _, h := range []string{"Accept", "User-Agent", "If-None-Match"} {
				if val := currentReq.Header.Get(h); val != "" {
					nextReq.Header.Set(h, val)
				}
			}
			currentReq = nextReq
			continue
		}

		break
	}

	defer resp.Body.Close()

	// 7. Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 8. Stream bounded response body directly without buffering into memory
	limitedBody := io.LimitReader(resp.Body, g.cfg.MaxBodyBytes)
	_, _ = io.Copy(w, limitedBody)
}

// Close gracefully closes the dependency gateway server, listener, and removes the Unix socket.
func (g *DependencyGateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.closed {
		return nil
	}
	g.closed = true

	onClose := g.cfg.OnClose
	g.cfg.OnClose = nil
	if onClose != nil {
		onClose()
	}

	var err error
	if g.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = g.server.Shutdown(ctx)
	}
	if g.listener != nil {
		lErr := g.listener.Close()
		if lErr != nil && !errors.Is(lErr, net.ErrClosed) && !strings.Contains(lErr.Error(), "use of closed network connection") {
			err = lErr
		}
	}
	if g.cfg.SocketPath != "" {
		_ = os.Remove(g.cfg.SocketPath)
	}
	return err
}

