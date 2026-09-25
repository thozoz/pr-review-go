package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
)

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
	want := []byte(s.cfg.GitHubAppSetupToken)
	got := []byte(value)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Server) handleGitHubAppSetup(w http.ResponseWriter, r *http.Request) {
	if !s.validSetupToken(r.URL.Query().Get("token")) {
		http.NotFound(w, r)
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
	action := "https://github.com/settings/apps/new?state=" + url.QueryEscape(s.cfg.GitHubAppSetupToken)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><title>pr-review-go setup</title><body><h1>Install pr-review-go GitHub App</h1><p>Creates private App with least required permissions.</p><form method="post" action="%s"><input type="hidden" name="manifest" value="%s"><button>Create GitHub App</button></form></body>`, template.HTMLEscapeString(action), template.HTMLEscapeString(string(encoded)))
}

func (s *Server) handleGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	if !s.validSetupToken(r.URL.Query().Get("state")) {
		http.NotFound(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "GitHub did not return setup code", http.StatusBadRequest)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://api.github.com/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		http.Error(w, "cannot create GitHub request", http.StatusInternalServerError)
		return
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
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
