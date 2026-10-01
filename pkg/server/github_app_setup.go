package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const setupStateTTL = 10 * time.Minute

// Architectural note on credentials display and replay protection:
// 1. Credentials Display: The GitHub App Manifest conversion API returns the App ID,
//    Webhook Secret, and private key PEM upon successful creation. Displaying these
//    credentials directly in the HTML response body (within read-only textareas) is an
//    application design choice to facilitate one-time initial setup without requiring an
//    external secrets manager. To mitigate leakage risks, responses are served with strict
//    no-store cache control, Content-Security-Policy, and anti-framing headers.
// 2. Replay Protection: Setup states are HMAC-SHA256 signed using GITHUB_APP_SETUP_TOKEN
//    with a 32-byte cryptographic nonce and timestamp (10-minute TTL). Single-use replay
//    protection is tracked in-memory per server process. Consequently, in multi-instance
//    deployments or across server restarts, replay prevention is enforced per-instance within
//    the TTL window; global cross-instance replay prevention would require distributed storage
//    (e.g., Redis), which is outside the scope of this lightweight daemon.

type setupStateStore struct {
	mu       sync.Mutex
	consumed map[string]time.Time // nonce -> expiration time
}

func newSetupStateStore() *setupStateStore {
	return &setupStateStore{
		consumed: make(map[string]time.Time),
	}
}

func (store *setupStateStore) cleanupExpired(now time.Time) {
	for nonce, exp := range store.consumed {
		if now.After(exp) {
			delete(store.consumed, nonce)
		}
	}
}

var (
	serverSetupStoresLock sync.Mutex
	serverSetupStores     = make(map[*Server]*setupStateStore)

	gitHubAppConversionMu          sync.RWMutex
	gitHubAppManifestConversionURL = "https://api.github.com/app-manifests/%s/conversions"
	gitHubAppHTTPClient            = http.DefaultClient
)

func (s *Server) getSetupStateStore() *setupStateStore {
	serverSetupStoresLock.Lock()
	defer serverSetupStoresLock.Unlock()
	store, ok := serverSetupStores[s]
	if !ok {
		store = newSetupStateStore()
		serverSetupStores[s] = store
	}
	return store
}

func getConversionURL(code string) string {
	gitHubAppConversionMu.RLock()
	defer gitHubAppConversionMu.RUnlock()
	return fmt.Sprintf(gitHubAppManifestConversionURL, url.PathEscape(code))
}

func getConversionClient() *http.Client {
	gitHubAppConversionMu.RLock()
	defer gitHubAppConversionMu.RUnlock()
	return gitHubAppHTTPClient
}

func setNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action https://github.com; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func computeSetupStateMAC(token, nonce string, ts int64) string {
	h := hmac.New(sha256.New, []byte(token))
	_, _ = h.Write([]byte("github-app-setup-state:"))
	_, _ = h.Write([]byte(nonce))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(strconv.FormatInt(ts, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) generateSetupState() (string, error) {
	if s.cfg.GitHubAppSetupToken == "" {
		return "", fmt.Errorf("github app setup token not configured")
	}
	nonceBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	ts := time.Now().Unix()
	mac := computeSetupStateMAC(s.cfg.GitHubAppSetupToken, nonce, ts)
	return fmt.Sprintf("%s.%d.%s", nonce, ts, mac), nil
}

func (s *Server) verifyAndConsumeSetupState(state string) bool {
	if s.cfg.GitHubAppSetupToken == "" || state == "" {
		return false
	}
	parts := strings.Split(state, ".")
	if len(parts) != 3 {
		return false
	}
	nonce, tsStr, gotMAC := parts[0], parts[1], parts[2]
	if len(nonce) != 64 || len(gotMAC) != 64 {
		return false
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return false
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	createdAt := time.Unix(ts, 0)
	now := time.Now()

	// Bounded lifetime (TTL) check
	if now.Sub(createdAt) > setupStateTTL || createdAt.After(now.Add(time.Minute)) {
		return false
	}

	// Constant-time HMAC verification
	expectedMAC := computeSetupStateMAC(s.cfg.GitHubAppSetupToken, nonce, ts)
	if subtle.ConstantTimeCompare([]byte(gotMAC), []byte(expectedMAC)) != 1 {
		return false
	}

	// Single-use replay protection
	store := s.getSetupStateStore()
	store.mu.Lock()
	defer store.mu.Unlock()

	store.cleanupExpired(now)

	if _, consumed := store.consumed[nonce]; consumed {
		return false
	}

	store.consumed[nonce] = createdAt.Add(setupStateTTL)
	return true
}

type githubAppManifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description"`
	Public             bool              `json:"public"`
	RedirectURL        string            `json:"redirect_url"`
	SetupURL           string            `json:"setup_url"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type manifestResult struct {
	ID            int64  `json:"id"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

func (s *Server) validSetupToken(value string) bool {
	if s.cfg.GitHubAppSetupToken == "" || value == "" {
		return false
	}
	want := []byte(s.cfg.GitHubAppSetupToken)
	got := []byte(value)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Server) handleGitHubAppSetup(w http.ResponseWriter, r *http.Request) {
	setNoCacheHeaders(w)
	if !s.validSetupToken(r.URL.Query().Get("token")) {
		http.NotFound(w, r)
		return
	}

	state, err := s.generateSetupState()
	if err != nil {
		http.Error(w, "cannot generate setup state", http.StatusInternalServerError)
		return
	}

	callback := s.cfg.PublicURL + "/setup/github-app/callback"
	manifest := githubAppManifest{
		Name:               "pr-review-go",
		URL:                "https://github.com/thozoz/pr-review-go",
		Description:        "Sandbox-verified PR reviewer",
		Public:             false,
		RedirectURL:        callback,
		SetupURL:           s.cfg.PublicURL,
		HookAttributes:     map[string]any{"url": s.cfg.PublicURL + "/api/v1/github_webhooks", "active": true},
		DefaultPermissions: map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read"},
		DefaultEvents:      []string{"pull_request", "issue_comment"},
	}
	encoded, _ := json.Marshal(manifest)
	action := "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><title>pr-review-go setup</title><body><h1>Install pr-review-go GitHub App</h1><p>Creates private App with least required permissions.</p><form method="post" action="%s"><input type="hidden" name="manifest" value="%s"><button>Create GitHub App</button></form></body>`, template.HTMLEscapeString(action), template.HTMLEscapeString(string(encoded)))
}

func (s *Server) handleGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	setNoCacheHeaders(w)
	state := r.URL.Query().Get("state")
	if !s.verifyAndConsumeSetupState(state) {
		http.NotFound(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "GitHub did not return setup code", http.StatusBadRequest)
		return
	}
	conversionURL := getConversionURL(code)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, conversionURL, nil)
	if err != nil {
		http.Error(w, "cannot create GitHub request", http.StatusInternalServerError)
		return
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := getConversionClient().Do(request)
	if err != nil {
		http.Error(w, "GitHub conversion failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2000))
		http.Error(w, "GitHub conversion failed: "+strings.TrimSpace(string(body)), http.StatusBadGateway)
		return
	}
	var result manifestResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		http.Error(w, "cannot decode GitHub response", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><title>GitHub App created</title><body><h1>GitHub App created</h1><p>Copy these once. Private key will not be shown again.</p><p>App: <a href="%s">%s</a></p><h2>App ID</h2><textarea readonly rows="1" cols="80">%d</textarea><h2>Webhook secret</h2><textarea readonly rows="2" cols="80">%s</textarea><h2>Private key PEM</h2><textarea readonly rows="20" cols="100">%s</textarea></body>`, template.HTMLEscapeString(result.HTMLURL), template.HTMLEscapeString(result.HTMLURL), result.ID, template.HTMLEscapeString(result.WebhookSecret), template.HTMLEscapeString(result.PEM))
}
