package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/config"
	"golang.org/x/oauth2"
)

const githubAPIURL = "https://api.github.com"

// AppAuth manages repository-scoped GitHub App installation authentication.
// It resolves installations per requested repository (rather than globally caching
// the first installation returned) and isolates token refreshes per owner/repo.
type AppAuth struct {
	appID         int64
	privateKey    *rsa.PrivateKey
	baseURL       string
	parsedBaseURL *url.URL
	httpClient    *http.Client
	earlyExpiry   time.Duration

	jwtMu     sync.Mutex
	cachedJWT string
	jwtExpiry time.Time

	mu    sync.Mutex
	repos map[string]*repoEntry
}

type repoEntry struct {
	mu   sync.Mutex
	inst *repoInstallation
}

type repoInstallation struct {
	owner          string
	repo           string
	installationID int64

	mu       sync.Mutex
	token    *oauth2.Token
	ghClient *gh.Client
}

type repoTokenSource struct {
	appAuth  *AppAuth
	repoInst *repoInstallation
}

func NewClientFromConfig(cfg *config.Config) *Client {
	if cfg.GitHubAppID == 0 {
		return NewClient(cfg.GitHubToken)
	}
	client, err := NewAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		panic(fmt.Sprintf("invalid GitHub App credentials: %v", err))
	}
	return client
}

func NewAppClient(appID int64, privateKeyPath string) (*Client, error) {
	data, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	key, err := parsePrivateKey(data)
	if err != nil {
		return nil, err
	}
	return newAppClient(appID, key, "")
}

// NewAppClientFromKey creates a GitHub App client from raw PEM private key bytes.
func NewAppClientFromKey(appID int64, keyData []byte) (*Client, error) {
	key, err := parsePrivateKey(keyData)
	if err != nil {
		return nil, err
	}
	return newAppClient(appID, key, "")
}

// NewTestAppClient creates an App-authenticated client pointed at a custom base URL for testing.
func NewTestAppClient(baseURL string, appID int64, privateKey *rsa.PrivateKey) (*Client, error) {
	return newAppClient(appID, privateKey, baseURL)
}

func newAppClient(appID int64, key *rsa.PrivateKey, baseURL string) (*Client, error) {
	if appID == 0 {
		return nil, fmt.Errorf("github app ID must not be zero")
	}
	if key == nil {
		return nil, fmt.Errorf("github app private key must not be nil")
	}
	var parsedBaseURL *url.URL
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid base URL %q: scheme and host required", baseURL)
		}
		if !strings.HasSuffix(u.Path, "/") {
			u.Path += "/"
		}
		parsedBaseURL = u
	}
	auth := newAppAuth(appID, key, baseURL, parsedBaseURL, http.DefaultClient)
	return &Client{appAuth: auth}, nil
}

func newAppAuth(appID int64, key *rsa.PrivateKey, baseURL string, parsedBaseURL *url.URL, httpClient *http.Client) *AppAuth {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AppAuth{
		appID:         appID,
		privateKey:    key,
		baseURL:       baseURL,
		parsedBaseURL: parsedBaseURL,
		httpClient:    httpClient,
		earlyExpiry:   5 * time.Minute,
		repos:         make(map[string]*repoEntry),
	}
}

func (a *AppAuth) apiURL() string {
	if a.parsedBaseURL != nil {
		return strings.TrimSuffix(a.parsedBaseURL.String(), "/")
	}
	return githubAPIURL
}

func (a *AppAuth) getJWT() (string, error) {
	a.jwtMu.Lock()
	defer a.jwtMu.Unlock()
	if a.cachedJWT != "" && time.Until(a.jwtExpiry) > 1*time.Minute {
		return a.cachedJWT, nil
	}
	jwt, err := a.signedJWT()
	if err != nil {
		return "", err
	}
	a.cachedJWT = jwt
	a.jwtExpiry = time.Now().Add(8 * time.Minute)
	return jwt, nil
}

func (a *AppAuth) signedJWT() (string, error) {
	now := time.Now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now - 60,
		"exp": now + 540,
		"iss": a.appID,
	})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *AppAuth) getRepoInstallationID(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := a.getJWT()
	if err != nil {
		return 0, fmt.Errorf("generate jwt: %w", err)
	}

	installURL := fmt.Sprintf("%s/repos/%s/%s/installation", a.apiURL(), url.PathEscape(owner), url.PathEscape(repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, installURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create installation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request installation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("github app is not installed on repository %s/%s (status 404)", owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("failed to get installation for %s/%s: status %d: %s", owner, repo, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var inst struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inst); err != nil {
		return 0, fmt.Errorf("decode installation response: %w", err)
	}
	if inst.ID == 0 {
		return 0, fmt.Errorf("no installation ID in response for %s/%s", owner, repo)
	}
	return inst.ID, nil
}

func (a *AppAuth) createInstallationToken(ctx context.Context, installationID int64) (*oauth2.Token, error) {
	jwt, err := a.getJWT()
	if err != nil {
		return nil, fmt.Errorf("generate jwt: %w", err)
	}

	tokenURL := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiURL(), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create access token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request access token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("failed to create access token for installation %d: status %d: %s", installationID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode access token response: %w", err)
	}
	if result.Token == "" {
		return nil, fmt.Errorf("empty access token returned for installation %d", installationID)
	}

	expiry := result.ExpiresAt
	if expiry.IsZero() {
		expiry = time.Now().Add(1 * time.Hour)
	}

	return &oauth2.Token{
		AccessToken: result.Token,
		TokenType:   "Bearer",
		Expiry:      expiry,
	}, nil
}

func (a *AppAuth) getClient(ctx context.Context, owner, repo string) (*gh.Client, error) {
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("owner and repo must not be empty for repository-scoped client")
	}
	repoKey := strings.ToLower(owner) + "/" + strings.ToLower(repo)

	// Acquire or allocate the per-repository entry under the global map lock.
	// Network I/O is explicitly kept outside this global lock so cold lookups on one
	// repository do not block cached or concurrent lookups on other repositories.
	a.mu.Lock()
	entry, ok := a.repos[repoKey]
	if !ok {
		entry = &repoEntry{}
		a.repos[repoKey] = entry
	}
	a.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.inst != nil {
		return entry.inst.ghClient, nil
	}

	installationID, err := a.getRepoInstallationID(ctx, owner, repo)
	if err != nil {
		a.mu.Lock()
		delete(a.repos, repoKey)
		a.mu.Unlock()
		return nil, fmt.Errorf("get installation for %s/%s: %w", owner, repo, err)
	}

	token, err := a.createInstallationToken(ctx, installationID)
	if err != nil {
		a.mu.Lock()
		delete(a.repos, repoKey)
		a.mu.Unlock()
		return nil, fmt.Errorf("create installation token for %s/%s (installation %d): %w", owner, repo, installationID, err)
	}

	repoInst := &repoInstallation{
		owner:          owner,
		repo:           repo,
		installationID: installationID,
		token:          token,
	}

	tokenSource := &repoTokenSource{
		appAuth:  a,
		repoInst: repoInst,
	}

	earlyExpiry := a.earlyExpiry
	if earlyExpiry <= 0 {
		earlyExpiry = 5 * time.Minute
	}

	// ReuseTokenSourceWithExpiry ensures proactive token refresh occurs whenever
	// the token is within earlyExpiry (default 5m) of expiration, rather than the default 10s.
	reuseSource := oauth2.ReuseTokenSourceWithExpiry(token, tokenSource, earlyExpiry)

	transport := http.DefaultTransport
	if a.httpClient != nil && a.httpClient.Transport != nil {
		transport = a.httpClient.Transport
	}

	httpClient := &http.Client{
		Transport: &oauth2.Transport{
			Source: reuseSource,
			Base:   transport,
		},
	}

	ghClient := gh.NewClient(httpClient)
	if a.parsedBaseURL != nil {
		ghClient.BaseURL = a.parsedBaseURL
	}

	repoInst.ghClient = ghClient
	entry.inst = repoInst

	return ghClient, nil
}

func (s *repoTokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token, err := s.appAuth.createInstallationToken(ctx, s.repoInst.installationID)
	if err != nil {
		return nil, fmt.Errorf("refresh token for %s/%s (installation %d): %w", s.repoInst.owner, s.repoInst.repo, s.repoInst.installationID, err)
	}

	s.repoInst.mu.Lock()
	s.repoInst.token = token
	s.repoInst.mu.Unlock()

	return token, nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return rsaKey, nil
}
