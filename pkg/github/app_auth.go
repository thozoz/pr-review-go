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
	"net/http"
	"os"
	"sync"
	"time"

	gh "github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/config"
	"golang.org/x/oauth2"
)

const githubAPIURL = "https://api.github.com"

type appTokenSource struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	httpClient     *http.Client
	mu             sync.Mutex
	token          *oauth2.Token
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
	source := &appTokenSource{appID: appID, privateKey: key, httpClient: http.DefaultClient}
	return &Client{gh: gh.NewClient(oauth2.NewClient(context.Background(), source))}, nil
}

func (s *appTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != nil && time.Until(s.token.Expiry) > 5*time.Minute {
		return s.token, nil
	}
	jwt, err := s.signedJWT()
	if err != nil {
		return nil, err
	}
	if s.installationID == 0 {
		req, _ := http.NewRequest(http.MethodGet, githubAPIURL+"/app/installations", nil)
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var installations []struct {
			ID int64 `json:"id"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&installations) != nil || len(installations) == 0 {
			return nil, fmt.Errorf("no GitHub App installation found")
		}
		s.installationID = installations[0].ID
	}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", githubAPIURL, s.installationID), nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&result) != nil || result.Token == "" {
		return nil, fmt.Errorf("failed to create GitHub App installation token")
	}
	s.token = &oauth2.Token{AccessToken: result.Token, TokenType: "Bearer", Expiry: result.ExpiresAt}
	return s.token, nil
}

func (s *appTokenSource) signedJWT() (string, error) {
	now := time.Now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 540, "iss": s.appID})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	input := header + "." + payload
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
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
