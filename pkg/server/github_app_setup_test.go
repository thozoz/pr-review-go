package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
)

func setupTestServer(setupToken string) *Server {
	cfg := &config.Config{
		Port:                3000,
		GitHubToken:         "test-token",
		LLMAPIKey:           "test-key",
		LLMModel:            "gpt-4o",
		LLMBaseURL:          "https://api.openai.com/v1",
		GitHubAppSetupToken: setupToken,
		PublicURL:           "https://review.example.com",
	}
	return NewServer(cfg)
}

func verifyNoCacheHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	cc := w.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") || !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control header missing required directives: %q", cc)
	}
	if pragma := w.Header().Get("Pragma"); pragma != "no-cache" {
		t.Errorf("Pragma header = %q, want 'no-cache'", pragma)
	}
	if expires := w.Header().Get("Expires"); expires != "0" {
		t.Errorf("Expires header = %q, want '0'", expires)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action https://github.com") {
		t.Errorf("Content-Security-Policy header missing required directives: %q", csp)
	}
	if xfo := w.Header().Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("X-Frame-Options header = %q, want 'DENY'", xfo)
	}
	if xcto := w.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options header = %q, want 'nosniff'", xcto)
	}
}

func TestGitHubAppSetupPage(t *testing.T) {
	setupToken := "super-secret-setup-token-12345"
	srv := setupTestServer(setupToken)

	t.Run("invalid token rejected with 404 and no-cache", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup/github-app?token=wrong-token", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("missing token rejected with 404 and no-cache", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup/github-app", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("valid token generates secure nonce state and does not leak setup token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup/github-app?token="+setupToken, nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		verifyNoCacheHeaders(t, w)

		body := w.Body.String()
		if strings.Contains(body, setupToken) {
			t.Fatalf("setup token leaked into setup page body or form action!")
		}

		// Extract state parameter from form action
		re := regexp.MustCompile(`action="https://github.com/settings/apps/new\?state=([^"]+)"`)
		matches := re.FindStringSubmatch(body)
		if len(matches) < 2 {
			t.Fatalf("form action with state parameter not found in body: %s", body)
		}
		state, err := url.QueryUnescape(matches[1])
		if err != nil {
			t.Fatalf("failed to unescape state: %v", err)
		}

		parts := strings.Split(state, ".")
		if len(parts) != 3 {
			t.Fatalf("state structure invalid, expected 3 parts, got %d: %q", len(parts), state)
		}
		if len(parts[0]) != 64 {
			t.Errorf("nonce length = %d, want 64", len(parts[0]))
		}
		if len(parts[2]) != 64 {
			t.Errorf("hmac length = %d, want 64", len(parts[2]))
		}
	})
}

func TestGitHubAppCallbackSecurity(t *testing.T) {
	setupToken := "super-secret-setup-token-67890"
	srv := setupTestServer(setupToken)

	t.Run("missing state rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?code=gh-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("static setup token as state rejected", func(t *testing.T) {
		// Verify that using the raw GITHUB_APP_SETUP_TOKEN as OAuth state is rejected
		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(setupToken)+"&code=gh-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("malformed state rejected", func(t *testing.T) {
		malformedStates := []string{
			"bad",
			"a.b",
			"a.b.c.d",
			"nonce.12345.mac",
			strings.Repeat("a", 64) + ".notanumber." + strings.Repeat("b", 64),
			strings.Repeat("a", 63) + ".12345." + strings.Repeat("b", 64),
			strings.Repeat("a", 64) + ".12345." + strings.Repeat("b", 63),
		}

		for _, badState := range malformedStates {
			req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(badState)+"&code=gh-code", nil)
			w := httptest.NewRecorder()
			srv.Routes().ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Errorf("malformed state %q status = %d, want 404", badState, w.Code)
			}
			verifyNoCacheHeaders(t, w)
		}
	})

	t.Run("forged state rejected", func(t *testing.T) {
		nonce := strings.Repeat("a", 64)
		ts := time.Now().Unix()
		forgedMAC := strings.Repeat("0", 64)
		forgedState := fmt.Sprintf("%s.%d.%s", nonce, ts, forgedMAC)

		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(forgedState)+"&code=gh-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("forged state status = %d, want 404", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("expired state rejected", func(t *testing.T) {
		nonce := strings.Repeat("c", 64)
		expiredTs := time.Now().Add(-11 * time.Minute).Unix()
		mac := computeSetupStateMAC(setupToken, nonce, expiredTs)
		expiredState := fmt.Sprintf("%s.%d.%s", nonce, expiredTs, mac)

		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(expiredState)+"&code=gh-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expired state status = %d, want 404", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("future state beyond skew tolerance rejected", func(t *testing.T) {
		nonce := strings.Repeat("d", 64)
		futureTs := time.Now().Add(5 * time.Minute).Unix()
		mac := computeSetupStateMAC(setupToken, nonce, futureTs)
		futureState := fmt.Sprintf("%s.%d.%s", nonce, futureTs, mac)

		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(futureState)+"&code=gh-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("future state status = %d, want 404", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})
}

func TestGitHubAppCallbackLifecycleAndReplay(t *testing.T) {
	setupToken := "test-lifecycle-token"
	srv := setupTestServer(setupToken)

	// Mock GitHub Conversion API
	mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") || !strings.HasSuffix(r.URL.Path, "/conversions") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("expected Accept application/vnd.github+json, got %s", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(manifestResult{
			ID:            98765,
			HTMLURL:       "https://github.com/apps/pr-review-go-test",
			PEM:           "-----BEGIN RSA PRIVATE KEY-----\nMIIEogIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----",
			WebhookSecret: "whsec_generated1234567890",
		})
	}))
	defer mockGitHub.Close()

	// Override conversion URL and client
	gitHubAppConversionMu.Lock()
	origURL := gitHubAppManifestConversionURL
	origClient := gitHubAppHTTPClient
	gitHubAppManifestConversionURL = mockGitHub.URL + "/app-manifests/%s/conversions"
	gitHubAppHTTPClient = mockGitHub.Client()
	gitHubAppConversionMu.Unlock()

	defer func() {
		gitHubAppConversionMu.Lock()
		gitHubAppManifestConversionURL = origURL
		gitHubAppHTTPClient = origClient
		gitHubAppConversionMu.Unlock()
	}()

	// 1. Visit setup page to obtain valid state
	setupReq := httptest.NewRequest(http.MethodGet, "/setup/github-app?token="+setupToken, nil)
	setupW := httptest.NewRecorder()
	srv.Routes().ServeHTTP(setupW, setupReq)
	if setupW.Code != http.StatusOK {
		t.Fatalf("setup page status = %d, want 200", setupW.Code)
	}

	re := regexp.MustCompile(`action="https://github.com/settings/apps/new\?state=([^"]+)"`)
	matches := re.FindStringSubmatch(setupW.Body.String())
	if len(matches) < 2 {
		t.Fatalf("could not extract state from setup form: %s", setupW.Body.String())
	}
	state, _ := url.QueryUnescape(matches[1])

	// 2. Callback with missing code returns 400 Bad Request and no-cache
	noCodeReq := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state), nil)
	noCodeW := httptest.NewRecorder()
	// Note: missing code happens AFTER state verification, which consumes the state!
	srv.Routes().ServeHTTP(noCodeW, noCodeReq)
	if noCodeW.Code != http.StatusBadRequest {
		t.Fatalf("missing code status = %d, want 400", noCodeW.Code)
	}
	verifyNoCacheHeaders(t, noCodeW)

	// 3. Attempting to replay that same state is rejected (404) because it's single-use
	replayReq := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state)+"&code=test-code", nil)
	replayW := httptest.NewRecorder()
	srv.Routes().ServeHTTP(replayW, replayReq)
	if replayW.Code != http.StatusNotFound {
		t.Fatalf("replayed state status = %d, want 404", replayW.Code)
	}
	verifyNoCacheHeaders(t, replayW)

	// 4. Generate another fresh state for full successful flow
	setupReq2 := httptest.NewRequest(http.MethodGet, "/setup/github-app?token="+setupToken, nil)
	setupW2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(setupW2, setupReq2)
	matches2 := re.FindStringSubmatch(setupW2.Body.String())
	state2, _ := url.QueryUnescape(matches2[1])

	// 5. Normal successful callback flow
	successReq := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state2)+"&code=valid-code", nil)
	successW := httptest.NewRecorder()
	srv.Routes().ServeHTTP(successW, successReq)

	if successW.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want 200; body: %s", successW.Code, successW.Body.String())
	}
	verifyNoCacheHeaders(t, successW)

	body := successW.Body.String()
	if !strings.Contains(body, "98765") {
		t.Errorf("response missing app id: %s", body)
	}
	if !strings.Contains(body, "whsec_generated1234567890") {
		t.Errorf("response missing webhook secret: %s", body)
	}
	if !strings.Contains(body, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Errorf("response missing private key PEM: %s", body)
	}

	// 6. Replaying the successfully completed state is rejected (404)
	replaySuccessReq := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state2)+"&code=valid-code", nil)
	replaySuccessW := httptest.NewRecorder()
	srv.Routes().ServeHTTP(replaySuccessW, replaySuccessReq)
	if replaySuccessW.Code != http.StatusNotFound {
		t.Fatalf("replayed successful state status = %d, want 404", replaySuccessW.Code)
	}
	verifyNoCacheHeaders(t, replaySuccessW)
}

func TestGitHubAppCallbackErrorHandling(t *testing.T) {
	setupToken := "error-test-token"
	srv := setupTestServer(setupToken)

	t.Run("github conversion failure returns 502 with no-cache", func(t *testing.T) {
		mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"GitHub internal error"}`))
		}))
		defer mockGitHub.Close()

		gitHubAppConversionMu.Lock()
		origURL := gitHubAppManifestConversionURL
		origClient := gitHubAppHTTPClient
		gitHubAppManifestConversionURL = mockGitHub.URL + "/app-manifests/%s/conversions"
		gitHubAppHTTPClient = mockGitHub.Client()
		gitHubAppConversionMu.Unlock()

		defer func() {
			gitHubAppConversionMu.Lock()
			gitHubAppManifestConversionURL = origURL
			gitHubAppHTTPClient = origClient
			gitHubAppConversionMu.Unlock()
		}()

		state, err := srv.generateSetupState()
		if err != nil {
			t.Fatalf("generateSetupState error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state)+"&code=fail-code", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})

	t.Run("invalid json from github returns 502 with no-cache", func(t *testing.T) {
		mockGitHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer mockGitHub.Close()

		gitHubAppConversionMu.Lock()
		origURL := gitHubAppManifestConversionURL
		origClient := gitHubAppHTTPClient
		gitHubAppManifestConversionURL = mockGitHub.URL + "/app-manifests/%s/conversions"
		gitHubAppHTTPClient = mockGitHub.Client()
		gitHubAppConversionMu.Unlock()

		defer func() {
			gitHubAppConversionMu.Lock()
			gitHubAppManifestConversionURL = origURL
			gitHubAppHTTPClient = origClient
			gitHubAppConversionMu.Unlock()
		}()

		state, err := srv.generateSetupState()
		if err != nil {
			t.Fatalf("generateSetupState error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/setup/github-app/callback?state="+url.QueryEscape(state)+"&code=bad-json", nil)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, req)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", w.Code)
		}
		verifyNoCacheHeaders(t, w)
	})
}

func TestSetupModeLifecycleAndHealthRoutes(t *testing.T) {
	// Setup mode with valid setup token and public URL, but missing LLM credentials
	// and an App ID pointing to a non-existent private key file.
	cfg := &config.Config{
		GitHubAppSetupToken:     "valid-setup-token-999",
		PublicURL:               "https://review.example.com",
		GitHubAppID:             12345,
		GitHubAppPrivateKeyPath: "/nonexistent/path/to/key.pem",
		LLMAPIKey:               "",
		LLMModel:                "",
		LLMBaseURL:              "",
		AutoActions:             []string{"review"},
	}

	// NewServer must not panic even with missing LLM credentials and unreadable private key
	var srv *Server
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewServer panicked in setup mode: %v", r)
		}
	}()
	srv = NewServer(cfg)
	if srv == nil {
		t.Fatalf("expected non-nil server")
	}

	routes := srv.Routes()

	// 1. GET /healthz must return 200 OK
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	wHealth := httptest.NewRecorder()
	routes.ServeHTTP(wHealth, reqHealth)
	if wHealth.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", wHealth.Code)
	}

	// 2. GET / must return 200 OK
	reqRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	wRoot := httptest.NewRecorder()
	routes.ServeHTTP(wRoot, reqRoot)
	if wRoot.Code != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", wRoot.Code)
	}

	// 3. POST /api/v1/github_webhooks must not be mounted in setup mode (returns 404)
	reqWebhook := httptest.NewRequest(http.MethodPost, "/api/v1/github_webhooks", nil)
	wWebhook := httptest.NewRecorder()
	routes.ServeHTTP(wWebhook, reqWebhook)
	if wWebhook.Code != http.StatusNotFound {
		t.Errorf("POST /api/v1/github_webhooks status = %d, want 404", wWebhook.Code)
	}
}
