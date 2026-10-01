package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	ghclient "github.com/thozoz/pr-review-go/pkg/github"
)

func signPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		Port:          3000,
		GitHubToken:   "test-token",
		LLMAPIKey:     "test-key",
		LLMModel:      "gpt-4o",
		LLMBaseURL:    "https://api.openai.com/v1",
		WebhookSecret: "",
	}
	srv := NewServer(cfg)

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	body := rr.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("expected response to contain 'ok', got %s", body)
	}
}

func TestWebhookPing(t *testing.T) {
	secret := "test-secret"
	cfg := &config.Config{
		Port:          3000,
		GitHubToken:   "test-token",
		LLMAPIKey:     "test-key",
		LLMModel:      "gpt-4o",
		LLMBaseURL:    "https://api.openai.com/v1",
		WebhookSecret: secret,
	}
	srv := NewServer(cfg)

	payload := []byte(`{"zen":"Keep it logically awesome."}`)
	req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))

	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
}

func TestWebhookSecurity(t *testing.T) {
	t.Run("missing secret webhook rejected and health unaffected", func(t *testing.T) {
		cfg := &config.Config{
			Port:          3000,
			GitHubToken:   "test-token",
			LLMAPIKey:     "test-key",
			LLMModel:      "gpt-4o",
			LLMBaseURL:    "https://api.openai.com/v1",
			WebhookSecret: "",
		}
		srv := NewServer(cfg)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", strings.NewReader(`{"zen":"Keep it logically awesome."}`))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected status 401 for missing webhook secret, got %d", rr.Code)
		}

		// Health endpoint unaffected
		healthReq := httptest.NewRequest("GET", "/healthz", nil)
		healthRR := httptest.NewRecorder()
		srv.Routes().ServeHTTP(healthRR, healthReq)

		if healthRR.Code != http.StatusOK {
			t.Fatalf("expected health status 200, got %d", healthRR.Code)
		}
		if !strings.Contains(healthRR.Body.String(), `"status":"ok"`) {
			t.Fatalf("expected health body to contain 'ok', got %s", healthRR.Body.String())
		}
	})

	t.Run("invalid HMAC rejected", func(t *testing.T) {
		cfg := &config.Config{
			Port:          3000,
			GitHubToken:   "test-token",
			LLMAPIKey:     "test-key",
			LLMModel:      "gpt-4o",
			LLMBaseURL:    "https://api.openai.com/v1",
			WebhookSecret: "correct-secret",
		}
		srv := NewServer(cfg)

		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", strings.NewReader(`{"zen":"Keep it logically awesome."}`))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", "sha256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expected status 401 for invalid HMAC, got %d", rr.Code)
		}
	})

	t.Run("valid signed ping accepted", func(t *testing.T) {
		secret := "correct-secret"
		cfg := &config.Config{
			Port:          3000,
			GitHubToken:   "test-token",
			LLMAPIKey:     "test-key",
			LLMModel:      "gpt-4o",
			LLMBaseURL:    "https://api.openai.com/v1",
			WebhookSecret: secret,
		}
		srv := NewServer(cfg)

		payload := []byte(`{"zen":"Keep it logically awesome."}`)
		req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", signPayload(secret, payload))
		rr := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200 for valid signed ping, got %d", rr.Code)
		}
	})
}

func TestCommentCommandPermissionAndImproveNotice(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		status     int
		wantNotice bool
	}{
		{"read-only comment denied", "read", http.StatusOK, false},
		{"permission API failure denied", "", http.StatusNotFound, false},
		{"writer sees unavailable notice", "write", http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notices := make(chan string, 1)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/org/repo/collaborators/member/permission":
					if tc.status != http.StatusOK {
						http.Error(w, "not found", tc.status)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]string{"permission": tc.permission})
				case "/repos/org/repo/issues/12/comments":
					var comment struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&comment); err != nil {
						t.Errorf("decode comment: %v", err)
					}
					notices <- comment.Body
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":1}`))
				default:
					t.Errorf("unexpected API request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			client, err := ghclient.NewTestClient(api.URL)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{GitHubToken: "test-token", LLMAPIKey: "test-key", LLMModel: "test-model", LLMBaseURL: "http://example.invalid", WebhookSecret: "secret", AutoActions: []string{}}
			srv := NewServer(cfg)
			srv.gh = client
			dispatched := make(chan string, 1)
			srv.dispatchHook = func(action, owner, repo string, prNum int) {
				dispatched <- action
			}
			payload := []byte(`{"action":"created","issue":{"number":12,"pull_request":{"url":"https://api.github.com/repos/org/repo/pulls/12"}},"comment":{"body":"/improve","user":{"login":"member"}},"repository":{"name":"repo","owner":{"login":"org"}}}`)
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Event", "issue_comment")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload("secret", payload))
			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("signed webhook status = %d, want 200", rr.Code)
			}
			if !tc.wantNotice {
				select {
				case act := <-dispatched:
					t.Fatalf("unauthorized comment dispatched action: %s", act)
				default:
				}
				if len(notices) != 0 {
					t.Fatalf("unauthorized comment produced notice: %s", <-notices)
				}
				return
			}
			select {
			case act := <-dispatched:
				if act != "improve" {
					t.Errorf("expected dispatched action 'improve', got %s", act)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("dispatch hook was not called for authorized user")
			}
			select {
			case notice := <-notices:
				if !strings.Contains(notice, "unavailable") {
					t.Errorf("unexpected improvement notice: %s", notice)
				}
			case <-time.After(5 * time.Second):
				t.Error("writer did not receive unavailable notice")
			}
		})
	}
}
