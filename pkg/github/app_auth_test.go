package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test RSA key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBlock := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: der,
	})
	return key, pemBlock
}

func generateTestPKCS8Key(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal PKCS8: %v", err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
	return key, pemBlock
}

// TestMultiOwnerRouting verifies that two distinct repositories/owners route to their
// respective installations and tokens independently without cross-talk or global caching.
func TestMultiOwnerRouting(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	var (
		installLookupCountOrgA int32
		installLookupCountOrgB int32
		tokenCreateCount1001   int32
		tokenCreateCount2002   int32
		prCallsOrgA            int32
		prCallsOrgB            int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		auth := r.Header.Get("Authorization")
		if auth == "" {
			http.Error(w, `{"message":"missing authorization"}`, http.StatusUnauthorized)
			return
		}

		switch r.URL.Path {
		case "/repos/org-a/repo-a/installation":
			atomic.AddInt32(&installLookupCountOrgA, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001})
			return

		case "/repos/org-b/repo-b/installation":
			atomic.AddInt32(&installLookupCountOrgB, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2002})
			return

		case "/app/installations/1001/access_tokens":
			atomic.AddInt32(&tokenCreateCount1001, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-for-org-a",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return

		case "/app/installations/2002/access_tokens":
			atomic.AddInt32(&tokenCreateCount2002, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-for-org-b",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return

		case "/repos/org-a/repo-a/pulls/1":
			if auth != "Bearer token-for-org-a" {
				http.Error(w, fmt.Sprintf(`{"message":"wrong token for org-a: %s"}`, auth), http.StatusForbidden)
				return
			}
			atomic.AddInt32(&prCallsOrgA, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 1,
				"title":  "PR from org-a",
				"base":   map[string]any{"ref": "main"},
				"head":   map[string]any{"ref": "feat-a", "sha": "sha-a"},
			})
			return

		case "/repos/org-b/repo-b/pulls/2":
			if auth != "Bearer token-for-org-b" {
				http.Error(w, fmt.Sprintf(`{"message":"wrong token for org-b: %s"}`, auth), http.StatusForbidden)
				return
			}
			atomic.AddInt32(&prCallsOrgB, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 2,
				"title":  "PR from org-b",
				"base":   map[string]any{"ref": "main"},
				"head":   map[string]any{"ref": "feat-b", "sha": "sha-b"},
			})
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 12345, key)
	if err != nil {
		t.Fatalf("failed to create test app client: %v", err)
	}

	ctx := context.Background()

	// 1. Fetch PR for org-a
	prA, err := client.GetPR(ctx, "org-a", "repo-a", 1)
	if err != nil {
		t.Fatalf("unexpected error fetching org-a PR: %v", err)
	}
	if prA.Title != "PR from org-a" {
		t.Fatalf("unexpected title for org-a: %s", prA.Title)
	}

	// 2. Fetch PR for org-b
	prB, err := client.GetPR(ctx, "org-b", "repo-b", 2)
	if err != nil {
		t.Fatalf("unexpected error fetching org-b PR: %v", err)
	}
	if prB.Title != "PR from org-b" {
		t.Fatalf("unexpected title for org-b: %s", prB.Title)
	}

	// 3. Repeated fetch to org-a: should reuse cached installation and token without re-querying
	prA2, err := client.GetPR(ctx, "org-a", "repo-a", 1)
	if err != nil {
		t.Fatalf("unexpected error fetching org-a PR again: %v", err)
	}
	if prA2.Title != "PR from org-a" {
		t.Fatalf("unexpected title on second call: %s", prA2.Title)
	}

	if atomic.LoadInt32(&installLookupCountOrgA) != 1 {
		t.Errorf("expected 1 installation lookup for org-a, got %d", installLookupCountOrgA)
	}
	if atomic.LoadInt32(&installLookupCountOrgB) != 1 {
		t.Errorf("expected 1 installation lookup for org-b, got %d", installLookupCountOrgB)
	}
	if atomic.LoadInt32(&tokenCreateCount1001) != 1 {
		t.Errorf("expected 1 token creation for org-a, got %d", tokenCreateCount1001)
	}
	if atomic.LoadInt32(&tokenCreateCount2002) != 1 {
		t.Errorf("expected 1 token creation for org-b, got %d", tokenCreateCount2002)
	}
	if atomic.LoadInt32(&prCallsOrgA) != 2 {
		t.Errorf("expected 2 PR calls for org-a, got %d", prCallsOrgA)
	}
	if atomic.LoadInt32(&prCallsOrgB) != 1 {
		t.Errorf("expected 1 PR call for org-b, got %d", prCallsOrgB)
	}
}

// TestTokenRefreshScoped verifies that token refresh for one repository does not
// cross-refresh or affect another repository's installation.
func TestTokenRefreshScoped(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	var (
		tokenIssueCounterOrgA int32
		tokenIssueCounterOrgB int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")

		switch r.URL.Path {
		case "/repos/org-a/repo-a/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001})
			return

		case "/repos/org-b/repo-b/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2002})
			return

		case "/app/installations/1001/access_tokens":
			idx := atomic.AddInt32(&tokenIssueCounterOrgA, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      fmt.Sprintf("token-org-a-v%d", idx),
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return

		case "/app/installations/2002/access_tokens":
			idx := atomic.AddInt32(&tokenIssueCounterOrgB, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      fmt.Sprintf("token-org-b-v%d", idx),
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return

		case "/repos/org-a/repo-a/pulls/1":
			if auth != "Bearer token-org-a-v1" && auth != "Bearer token-org-a-v2" {
				http.Error(w, fmt.Sprintf(`{"message":"unexpected token for org-a: %s"}`, auth), http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "PR 1"})
			return

		case "/repos/org-b/repo-b/pulls/2":
			if auth != "Bearer token-org-b-v1" {
				http.Error(w, fmt.Sprintf(`{"message":"unexpected token for org-b: %s"}`, auth), http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "title": "PR 2"})
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 999, key)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Initial requests for both
	_, err = client.GetPR(ctx, "org-a", "repo-a", 1)
	if err != nil {
		t.Fatalf("org-a initial failed: %v", err)
	}
	_, err = client.GetPR(ctx, "org-b", "repo-b", 2)
	if err != nil {
		t.Fatalf("org-b initial failed: %v", err)
	}

	if atomic.LoadInt32(&tokenIssueCounterOrgA) != 1 {
		t.Errorf("expected 1 token for org-a, got %d", tokenIssueCounterOrgA)
	}
	if atomic.LoadInt32(&tokenIssueCounterOrgB) != 1 {
		t.Errorf("expected 1 token for org-b, got %d", tokenIssueCounterOrgB)
	}

	// 2. Expire token for org-a only to test isolated refresh
	client.appAuth.mu.Lock()
	entryA := client.appAuth.repos["org-a/repo-a"]
	client.appAuth.mu.Unlock()

	entryA.inst.mu.Lock()
	entryA.inst.token.Expiry = time.Now().Add(-1 * time.Minute)
	entryA.inst.mu.Unlock()

	// Next request for org-a should trigger refresh (since expiry < 5m)
	_, err = client.GetPR(ctx, "org-a", "repo-a", 1)
	if err != nil {
		t.Fatalf("org-a second call failed: %v", err)
	}

	if atomic.LoadInt32(&tokenIssueCounterOrgA) != 2 {
		t.Errorf("expected org-a token to refresh, count=%d", tokenIssueCounterOrgA)
	}

	// 3. Request for org-b should NOT trigger refresh (still valid for 1 hour)
	_, err = client.GetPR(ctx, "org-b", "repo-b", 2)
	if err != nil {
		t.Fatalf("org-b second call failed: %v", err)
	}

	if atomic.LoadInt32(&tokenIssueCounterOrgB) != 1 {
		t.Errorf("expected org-b token NOT to refresh, count=%d", tokenIssueCounterOrgB)
	}
}

// TestTokenPreExpiryWindow verifies that ReuseTokenSourceWithExpiry proactive 5-minute
// window triggers a refresh when the token is within 5 minutes of expiration.
func TestTokenPreExpiryWindow(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	var tokenCallCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/pre-expire/repo/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 8888})
			return
		case "/app/installations/8888/access_tokens":
			c := atomic.AddInt32(&tokenCallCount, 1)
			if c == 1 {
				// Return token expiring in 3 minutes (within the 5m earlyExpiry buffer)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":      "token-v1-preexpire",
					"expires_at": time.Now().Add(3 * time.Minute),
				})
				return
			}
			// Second token valid for 1 hour
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-v2-fresh",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return
		case "/repos/pre-expire/repo/pulls/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "PR"})
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 123, key)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// First PR call: getClient initializes with token-v1 expiring in 3m.
	// ReuseTokenSourceWithExpiry recognizes that 3m < 5m buffer and immediately calls
	// TokenSource to refresh, obtaining token-v2.
	_, err = client.GetPR(context.Background(), "pre-expire", "repo", 1)
	if err != nil {
		t.Fatalf("first PR call failed: %v", err)
	}

	// Should have refreshed proactively because token-v1 was within the 5-minute pre-expiry window
	if calls := atomic.LoadInt32(&tokenCallCount); calls != 2 {
		t.Fatalf("expected proactive refresh on 3m expiry token (count=2), got count=%d", calls)
	}

	// Subsequent call within 1 hour should reuse token-v2 without extra refresh
	_, err = client.GetPR(context.Background(), "pre-expire", "repo", 1)
	if err != nil {
		t.Fatalf("second PR call failed: %v", err)
	}
	if calls := atomic.LoadInt32(&tokenCallCount); calls != 2 {
		t.Fatalf("expected cached token-v2 reuse (count=2), got count=%d", calls)
	}
}

// TestConcurrentCachedNotBlockedBySlowColdLookup verifies that cold installation lookup on one
// repository does not hold a global lock and therefore does not block requests to an already-cached repository.
func TestConcurrentCachedNotBlockedBySlowColdLookup(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	coldLookupStarted := make(chan struct{})
	coldLookupRelease := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/warm-org/warm-repo/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1111})
			return
		case "/app/installations/1111/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-warm",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return
		case "/repos/warm-org/warm-repo/pulls/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "Warm PR"})
			return

		case "/repos/cold-org/cold-repo/installation":
			close(coldLookupStarted)
			<-coldLookupRelease
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2222})
			return
		case "/app/installations/2222/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-cold",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return
		case "/repos/cold-org/cold-repo/pulls/2":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "title": "Cold PR"})
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 777, key)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Prime the warm repository in cache
	warmPR, err := client.GetPR(ctx, "warm-org", "warm-repo", 1)
	if err != nil || warmPR.Title != "Warm PR" {
		t.Fatalf("failed to prime warm repo: %v", err)
	}

	// 2. Start cold lookup in a background goroutine which will block on coldLookupRelease
	coldErrCh := make(chan error, 1)
	go func() {
		_, err := client.GetPR(ctx, "cold-org", "cold-repo", 2)
		coldErrCh <- err
	}()

	// Wait until the cold lookup is actively waiting inside the HTTP handler
	<-coldLookupStarted

	// 3. Request the cached warm repository while cold lookup is blocked
	warmStart := time.Now()
	warmPR2, err := client.GetPR(ctx, "warm-org", "warm-repo", 1)
	warmDuration := time.Since(warmStart)

	if err != nil || warmPR2.Title != "Warm PR" {
		t.Fatalf("warm repo lookup failed during concurrent cold lookup: %v", err)
	}
	if warmDuration > 200*time.Millisecond {
		t.Errorf("warm repo lookup was unexpectedly delayed (%v), indicating global mutex lock contention", warmDuration)
	}

	// 4. Release the cold lookup and ensure it finishes
	close(coldLookupRelease)
	if err := <-coldErrCh; err != nil {
		t.Fatalf("cold lookup failed after release: %v", err)
	}
}

// TestNewAppClient_InvalidBaseURL verifies that malformed baseURLs fail early instead of
// silently falling back to public api.github.com.
func TestNewAppClient_InvalidBaseURL(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	invalidURLs := []string{
		"://missing-scheme",
		"ftp://",
		"http://",
		":///invalid",
	}

	for _, u := range invalidURLs {
		_, err := NewTestAppClient(u, 12345, key)
		if err == nil {
			t.Errorf("expected NewTestAppClient(%q) to fail early, got nil error", u)
		}
	}

	// Valid baseURL succeeds
	validClient, err := NewTestAppClient("http://127.0.0.1:8080", 12345, key)
	if err != nil || validClient == nil {
		t.Fatalf("expected valid URL to succeed, got %v", err)
	}
}

// TestTrimOwnerRepo verifies that whitespace in owner and repo is trimmed before installation lookup
// and matches the same normalized cache entry.
func TestTrimOwnerRepo(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	var lookupCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch strings.ToLower(r.URL.Path) {
		case "/repos/clean-org/clean-repo/installation":
			atomic.AddInt32(&lookupCount, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5555})
			return
		case "/app/installations/5555/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "token-trimmed",
				"expires_at": time.Now().Add(1 * time.Hour),
			})
			return
		case "/repos/clean-org/clean-repo/pulls/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "Trimmed PR"})
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 999, key)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Untrimmed input
	pr1, err := client.GetPR(ctx, "  clean-org  ", "  clean-repo\t", 1)
	if err != nil {
		t.Fatalf("untrimmed PR call failed: %v", err)
	}
	if pr1.Title != "Trimmed PR" {
		t.Fatalf("expected title 'Trimmed PR', got %s", pr1.Title)
	}

	// 2. Different casing and trimmed input should reuse the same cached entry
	pr2, err := client.GetPR(ctx, "CLEAN-ORG", "CLEAN-REPO", 1)
	if err != nil {
		t.Fatalf("cased PR call failed: %v", err)
	}
	if pr2.Title != "Trimmed PR" {
		t.Fatalf("expected title 'Trimmed PR', got %s", pr2.Title)
	}

	if count := atomic.LoadInt32(&lookupCount); count != 1 {
		t.Errorf("expected 1 installation lookup for trimmed repo, got %d", count)
	}
}

// TestAPIErrorsFailClosed verifies that uninstalled apps, network failures, or invalid responses
// fail closed with clear errors and never allow unauthorized access.
func TestAPIErrorsFailClosed(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/uninstalled/repo/installation":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return

		case "/repos/server-err/repo/installation":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
			return

		case "/repos/bad-token/repo/installation":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 3003})
			return

		case "/app/installations/3003/access_tokens":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"Bad Gateway"}`))
			return

		case "/repos/empty-token/repo/installation":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4004})
			return

		case "/app/installations/4004/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": ""})
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewTestAppClient(server.URL, 54321, key)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Not installed (404)
	_, err = client.GetPR(ctx, "uninstalled", "repo", 1)
	if err == nil {
		t.Fatalf("expected error for uninstalled repository, got nil")
	}

	// 2. Server error on installation lookup (500)
	_, err = client.GetPR(ctx, "server-err", "repo", 1)
	if err == nil {
		t.Fatalf("expected error for 500 installation lookup, got nil")
	}

	// 3. Bad gateway on token creation
	_, err = client.GetPR(ctx, "bad-token", "repo", 1)
	if err == nil {
		t.Fatalf("expected error for failed token creation, got nil")
	}

	// 4. Empty token returned
	_, err = client.GetPR(ctx, "empty-token", "repo", 1)
	if err == nil {
		t.Fatalf("expected error for empty token response, got nil")
	}

	// 5. Empty owner or repo fails closed immediately
	_, err = client.GetPR(ctx, "", "repo", 1)
	if err == nil {
		t.Fatalf("expected error for empty owner, got nil")
	}
	_, err = client.GetPR(ctx, "owner", "", 1)
	if err == nil {
		t.Fatalf("expected error for empty repo, got nil")
	}
}

// TestPATPathUnchanged verifies that standard personal access token (PAT) clients
// make direct requests with their static token and perform no App installation lookups.
func TestPATPathUnchanged(t *testing.T) {
	var appEndpointCalled int32
	var prCalls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")

		if r.URL.Path == "/app/installations" || r.URL.Path == "/repos/my-org/my-repo/installation" {
			atomic.AddInt32(&appEndpointCalled, 1)
			http.Error(w, "should not be called", http.StatusInternalServerError)
			return
		}

		if r.URL.Path == "/repos/my-org/my-repo/pulls/10" {
			if auth != "Bearer my-pat-token" {
				http.Error(w, fmt.Sprintf("unexpected auth header: %s", auth), http.StatusForbidden)
				return
			}
			atomic.AddInt32(&prCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 10,
				"title":  "PAT PR",
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	patClient, err := NewTestClientWithToken(server.URL, "my-pat-token")
	if err != nil {
		t.Fatalf("failed to create PAT test client: %v", err)
	}

	prResult, err := patClient.GetPR(context.Background(), "my-org", "my-repo", 10)
	if err != nil {
		t.Fatalf("unexpected error for PAT client: %v", err)
	}
	if prResult.Title != "PAT PR" {
		t.Fatalf("expected title 'PAT PR', got %s", prResult.Title)
	}

	if atomic.LoadInt32(&appEndpointCalled) != 0 {
		t.Errorf("App installation endpoint should not have been called for PAT client")
	}
	if atomic.LoadInt32(&prCalls) != 1 {
		t.Errorf("expected 1 PR call, got %d", prCalls)
	}
}

func TestParsePrivateKey_Formats(t *testing.T) {
	_, pkcs1PEM := generateTestRSAKey(t)
	_, pkcs8PEM := generateTestPKCS8Key(t)

	key1, err := parsePrivateKey(pkcs1PEM)
	if err != nil || key1 == nil {
		t.Fatalf("failed to parse PKCS1 key: %v", err)
	}

	key2, err := parsePrivateKey(pkcs8PEM)
	if err != nil || key2 == nil {
		t.Fatalf("failed to parse PKCS8 key: %v", err)
	}

	_, err = parsePrivateKey([]byte("invalid pem data"))
	if err == nil {
		t.Fatalf("expected error for invalid PEM data, got nil")
	}
}

func TestClientCacheEvictionAndOrphaningRegression(t *testing.T) {
	key, _ := generateTestRSAKey(t)

	var installCalls int32
	var tokenCalls int32

	g1InReq := make(chan struct{})
	g1Release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/test-org/test-repo/installation":
			callNum := atomic.AddInt32(&installCalls, 1)
			if callNum == 1 {
				close(g1InReq)
				select {
				case <-g1Release:
				case <-time.After(5 * time.Second):
					t.Errorf("timeout waiting for g1Release in server handler")
				}
				http.Error(w, `{"message":"temporary upstream error"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 555,
			})
			return

		case r.URL.Path == "/app/installations/555/access_tokens":
			atomic.AddInt32(&tokenCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "tok-regression-555",
				"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	appClient, err := NewTestAppClient(server.URL, 999, key)
	if err != nil {
		t.Fatalf("failed to create test app client: %v", err)
	}
	auth := appClient.appAuth

	ctx := context.Background()
	repoKey := "test-org/test-repo"

	type result struct {
		client any
		err    error
	}

	resG1 := make(chan result, 1)
	resG2 := make(chan result, 1)

	var callCount int32
	g1Selected := make(chan *repoEntry, 1)
	g2Selected := make(chan *repoEntry, 2)
	auth.onEntrySelected = func(rk string, e *repoEntry) {
		if rk != repoKey {
			return
		}
		c := atomic.AddInt32(&callCount, 1)
		if c == 1 {
			g1Selected <- e
		} else {
			g2Selected <- e
		}
	}

	// Goroutine 1: will enter getClient first, hold entry1.mu, and hit install endpoint
	go func() {
		c, err := auth.getClient(ctx, "test-org", "test-repo")
		resG1 <- result{client: c, err: err}
	}()

	var entry1 *repoEntry
	select {
	case entry1 = <-g1Selected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G1 entry selection")
	}

	// Wait until G1 is actively inside the HTTP request holding entry1.mu
	select {
	case <-g1InReq:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G1 in HTTP request")
	}

	auth.mu.Lock()
	mapEntry1 := auth.repos[repoKey]
	auth.mu.Unlock()
	if mapEntry1 == nil || mapEntry1 != entry1 {
		t.Fatalf("expected entry1 to be registered in auth.repos")
	}

	// Goroutine 2: starts while G1 still holds entry1.mu
	go func() {
		c, err := auth.getClient(ctx, "test-org", "test-repo")
		resG2 <- result{client: c, err: err}
	}()

	// Synchronously verify that G2 obtained entry1 before G1 is released
	var g2InitialEntry *repoEntry
	select {
	case g2InitialEntry = <-g2Selected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G2 entry selection")
	}
	if g2InitialEntry != entry1 {
		t.Fatalf("expected G2 to select entry1 while G1 holds it, got %p != %p", g2InitialEntry, entry1)
	}

	// Now that G2 is guaranteed to have obtained entry1 and is waiting on entry1.mu,
	// release G1 so it fails with 500 and evicts entry1
	close(g1Release)

	var r1 result
	select {
	case r1 = <-resG1:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G1 result")
	}
	if r1.err == nil {
		t.Fatalf("expected G1 to fail with error, got success")
	}

	// G2 detects entry1 is stale, releases entry1.mu, and retries with a fresh entry
	var g2RetryEntry *repoEntry
	select {
	case g2RetryEntry = <-g2Selected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G2 retry entry selection")
	}
	if g2RetryEntry == entry1 {
		t.Fatalf("expected G2 retry entry to be new, but got stale entry1")
	}

	// G2 must succeed after retry
	var r2 result
	select {
	case r2 = <-resG2:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for G2 result")
	}
	if r2.err != nil {
		t.Fatalf("expected G2 to succeed after retry, got: %v", r2.err)
	}
	if r2.client == nil {
		t.Fatalf("expected G2 client to be non-nil")
	}

	auth.mu.Lock()
	entryInMap := auth.repos[repoKey]
	auth.mu.Unlock()
	if entryInMap == nil || entryInMap != g2RetryEntry {
		t.Fatalf("expected g2RetryEntry to be in repos map")
	}

	// Verify that stale eviction attempt with entry1 CANNOT delete active retry entry
	auth.evictEntry(repoKey, entry1)

	auth.mu.Lock()
	entryAfterStaleEvict := auth.repos[repoKey]
	auth.mu.Unlock()
	if entryAfterStaleEvict != g2RetryEntry {
		t.Fatalf("stale eviction deleted active retry entry from repos map")
	}

	// Goroutine 3: new call must share the cached client without additional API calls
	clientG3, err := auth.getClient(ctx, "test-org", "test-repo")
	if err != nil {
		t.Fatalf("G3 getClient failed: %v", err)
	}
	if clientG3 != r2.client {
		t.Fatalf("G3 did not share the cached client from G2")
	}

	if atomic.LoadInt32(&installCalls) != 2 {
		t.Errorf("expected exactly 2 install calls (1 failed G1, 1 successful G2), got %d", installCalls)
	}
	if atomic.LoadInt32(&tokenCalls) != 1 {
		t.Errorf("expected exactly 1 token call (shared with G3), got %d", tokenCalls)
	}
}
