package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		{"triage comment denied", "triage", http.StatusOK, false},
		{"none permission denied", "none", http.StatusOK, false},
		{"empty permission denied", "", http.StatusOK, false},
		{"permission API failure 404 denied", "", http.StatusNotFound, false},
		{"permission API failure 500 denied", "", http.StatusInternalServerError, false},
		{"permission API failure 401 denied", "", http.StatusUnauthorized, false},
		{"permission API failure 403 denied", "", http.StatusForbidden, false},
		{"writer sees unavailable notice", "write", http.StatusOK, true},
		{"admin sees unavailable notice", "admin", http.StatusOK, true},
		{"maintainer sees unavailable notice", "maintain", http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notices := make(chan string, 1)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/org/repo/collaborators/member/permission":
					if tc.status != http.StatusOK {
						http.Error(w, "permission check error", tc.status)
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
			cfg := &config.Config{
				GitHubToken:     "test-token",
				LLMAPIKey:       "test-key",
				LLMModel:        "test-model",
				LLMBaseURL:      "http://example.invalid",
				WebhookSecret:   "secret",
				WebhookStateDir: t.TempDir(),
				AutoActions:     []string{},
			}
			srv := NewServer(cfg)
			defer srv.Stop()
			srv.gh = client
			dispatched := make(chan string, 1)
			srv.dispatchHook = func(action, owner, repo string, prNum int) {
				dispatched <- action
			}
			payload := []byte(`{"action":"created","issue":{"number":12,"pull_request":{"url":"https://api.github.com/repos/org/repo/pulls/12"}},"comment":{"id":101,"body":"/improve","user":{"login":"member"}},"repository":{"id":12345,"name":"repo","owner":{"login":"org"}}}`)
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-%d", time.Now().UnixNano()))
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

func TestCommentCommands_UnauthorizedCommandsDenied(t *testing.T) {
	commands := []string{
		"/review", "/describe", "/update_changelog", "/generate_labels",
		"/labels", "/summarize", "/summary", "/add_docs", "/docs",
		"@bot please explain", "/ask how does this work",
	}

	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/org/repo/collaborators/untrusted/permission" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]string{"permission": "read"})
					return
				}
				http.NotFound(w, r)
			}))
			defer api.Close()

			client, err := ghclient.NewTestClient(api.URL)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{
				GitHubToken:     "test-token",
				LLMAPIKey:       "test-key",
				LLMModel:        "test-model",
				LLMBaseURL:      "http://example.invalid",
				WebhookSecret:   "secret",
				WebhookStateDir: t.TempDir(),
			}
			srv := NewServer(cfg)
			defer srv.Stop()
			srv.gh = client
			dispatched := make(chan string, 1)
			srv.dispatchHook = func(action, owner, repo string, prNum int) {
				dispatched <- action
			}

			payload := []byte(fmt.Sprintf(`{"action":"created","issue":{"number":5,"pull_request":{"url":"https://api.github.com/repos/org/repo/pulls/5"}},"comment":{"id":102,"body":%q,"user":{"login":"untrusted"}},"repository":{"id":12345,"name":"repo","owner":{"login":"org"}}}`, cmd))
			req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("deliv-%d", time.Now().UnixNano()))
			req.Header.Set("X-GitHub-Event", "issue_comment")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", signPayload("secret", payload))
			rr := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("webhook status = %d, want 200", rr.Code)
			}

			select {
			case act := <-dispatched:
				t.Fatalf("unauthorized user command %q should NOT have dispatched, but dispatched: %s", cmd, act)
			default:
				// success: nothing dispatched
			}
		})
	}
}

func TestParseCommentCommand(t *testing.T) {
	cases := []struct {
		body        string
		wantKind    string
		wantPayload string
		wantCmd     bool
	}{
		{"/approve", "approve", "", true},
		{"/approve please merge", "approve", "", true},
		{"/approve\nlooks good", "approve", "", true},
		{"/request_changes", "request_changes", "", true},
		{"/request_changes fix formatting", "request_changes", "", true},
		{"/review", "review", "", true},
		{"/review all", "review", "", true},
		{"/improve", "improve", "", true},
		{"/improve --commit fix typo", "edit", "fix typo", true},
		{"/improve do not use --commit-flag", "improve", "", true},
		{"@pr-review rate limiter ekle", "edit", "rate limiter ekle", true},
		// Near-miss prefixes staying unrecognized
		{"/approving", "", "", false},
		{"/appr", "", "", false},
		{"/requested_changes", "", "", false},
		{"/request_change", "", "", false},
		{"/reviewer", "review", "", true},
		{"hello world", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			kind, payload := parseCommentCommand(tc.body)
			if kind != tc.wantKind {
				t.Errorf("parseCommentCommand(%q) kind = %q, want %q", tc.body, kind, tc.wantKind)
			}
			if tc.wantPayload != "" && payload != tc.wantPayload {
				t.Errorf("parseCommentCommand(%q) payload = %q, want %q", tc.body, payload, tc.wantPayload)
			}
			isCmd := isCommentCommand(tc.body)
			if isCmd != tc.wantCmd {
				t.Errorf("isCommentCommand(%q) = %v, want %v", tc.body, isCmd, tc.wantCmd)
			}
		})
	}
}

func TestAdmitDecision_CreatesStatusIntent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "admit_decision.db")
	store, err := OpenJobStore(dbPath, StoreOptions{BacklogLimit: 10})
	if err != nil {
		t.Fatalf("failed opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	prKey, _ := MakePRKey("github.com", 1234, 1)
	const validHead = "0123456789abcdef0123456789abcdef01234567"

	job := Job{
		ID:        "job-approve-1",
		Kind:      "approve",
		Trigger:   "explicit",
		Author:    "alice",
		CommentID: 101,
		PRKey:     prKey,
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  1,
		HeadSHA:   validHead,
	}

	delivery := Delivery{
		Host:        "github.com",
		RepoID:      1234,
		DeliveryID:  "deliv-app-1",
		EventKind:   "issue_comment",
		PayloadHash: "abc",
		ReceivedAt:  time.Now().UTC(),
	}

	res, err := store.Admit(ctx, delivery, []Job{job})
	if err != nil {
		t.Fatalf("unexpected admit error: %v", err)
	}
	if res.Status != AdmitAccepted {
		t.Fatalf("expected AdmitAccepted, got %v", res.Status)
	}

	queuedJobs, err := store.ListQueuedJobs(ctx)
	if err != nil || len(queuedJobs) == 0 {
		t.Fatalf("expected queued job, got %v", err)
	}
	admittedJob := queuedJobs[0]

	// Verify status intent was created in store (D-16)
	marker := fmt.Sprintf("<!-- pr-review-status:%s -->", admittedJob.ID)
	intent, err := store.GetOutputIntent(ctx, marker)
	if err != nil {
		t.Fatalf("expected status intent to exist: %v", err)
	}
	if intent == nil || intent.JobID != admittedJob.ID || intent.ExactHead != validHead {
		t.Fatalf("unexpected intent state: %+v", intent)
	}
}

func TestAdmitDecision_AutomaticTriggerCannotAdmitDecision(t *testing.T) {
	// D-01: No automatic trigger may admit an approve or request_changes job
	cfg := &config.Config{
		Port:          3000,
		GitHubToken:   "test-token",
		LLMAPIKey:     "test-key",
		LLMModel:      "gpt-4o",
		LLMBaseURL:    "https://api.openai.com/v1",
		WebhookSecret: "secret",
		AutoActions:   []string{"review"},
	}
	srv := NewServer(cfg)
	defer srv.Stop()

	payload := []byte(`{"action":"opened","pull_request":{"number":10,"head":{"sha":"0123456789abcdef0123456789abcdef01234567"},"base":{"sha":"1111111111111111111111111111111111111111"}},"repository":{"id":999,"name":"repo","owner":{"login":"org"}}}`)
	req := httptest.NewRequest("POST", "/api/v1/github_webhooks", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Delivery", "deliv-pr-open")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", signPayload("secret", payload))
	rr := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	jobs, err := srv.store.ListQueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("failed listing queued jobs: %v", err)
	}

	for _, j := range jobs {
		if j.Kind == "approve" || j.Kind == "request_changes" {
			t.Fatalf("D-01 violation: automatic pull_request event admitted decision job kind %q", j.Kind)
		}
	}
}
