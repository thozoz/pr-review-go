package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		url        string
		wantOwner  string
		wantRepo   string
		wantNumber int
		wantErr    bool
	}{
		{
			url:        "https://github.com/thozoz/pr-review-go/pull/42",
			wantOwner:  "thozoz",
			wantRepo:   "pr-review-go",
			wantNumber: 42,
			wantErr:    false,
		},
		{
			url:        "http://github.com/golang/go/pull/100",
			wantOwner:  "golang",
			wantRepo:   "go",
			wantNumber: 100,
			wantErr:    false,
		},
		{
			url:     "https://github.com/invalid/url",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		owner, repo, num, err := ParsePRURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParsePRURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if !tt.wantErr {
			if owner != tt.wantOwner || repo != tt.wantRepo || num != tt.wantNumber {
				t.Errorf("ParsePRURL(%q) = (%s, %s, %d), want (%s, %s, %d)",
					tt.url, owner, repo, num, tt.wantOwner, tt.wantRepo, tt.wantNumber)
			}
		}
	}
}

func TestGetPR_HeadRepoOwner(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/my-org/my-repo/pulls/42" {
			prJSON := map[string]any{
				"number": 42,
				"title":  "A fork PR",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "my-org"},
						"name":  "my-repo",
					},
				},
				"head": map[string]any{
					"ref": "feature",
					"sha": "sha-abc",
					"repo": map[string]any{
						"owner":     map[string]any{"login": "contributor-org"},
						"name":      "my-repo",
						"clone_url": "https://github.com/contributor-org/my-repo.git",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	pr, err := client.GetPR(context.Background(), "my-org", "my-repo", 42)
	if err != nil {
		t.Fatalf("unexpected error from GetPR: %v", err)
	}

	if pr.HeadRepoOwner != "contributor-org" {
		t.Errorf("expected HeadRepoOwner to be %q, got %q", "contributor-org", pr.HeadRepoOwner)
	}
	if pr.HeadRepoName != "my-repo" {
		t.Errorf("expected HeadRepoName to be %q, got %q", "my-repo", pr.HeadRepoName)
	}
	if !pr.IsFork() {
		t.Errorf("expected IsFork() to be true for contributor-org != my-org")
	}
	if pr.HeadSHA != "sha-abc" {
		t.Errorf("expected HeadSHA to be %q, got %q", "sha-abc", pr.HeadSHA)
	}
	if pr.HeadRef != "feature" {
		t.Errorf("expected HeadRef to be %q, got %q", "feature", pr.HeadRef)
	}
}

func TestGetPR_NullHeadRepo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/my-org/my-repo/pulls/99" {
			prJSON := map[string]any{
				"number": 99,
				"title":  "PR with deleted head repo",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "my-org"},
						"name":  "my-repo",
					},
				},
				"head": map[string]any{
					"ref":  "deleted-branch",
					"sha":  "sha-deleted",
					"repo": nil,
				},
			}
			json.NewEncoder(w).Encode(prJSON)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	pr, err := client.GetPR(context.Background(), "my-org", "my-repo", 99)
	if err != nil {
		t.Fatalf("unexpected error from GetPR with null head repo: %v", err)
	}

	if pr.HeadRepoOwner != "" {
		t.Errorf("expected empty HeadRepoOwner for null head repo, got %q", pr.HeadRepoOwner)
	}
	if pr.HeadRepoName != "" {
		t.Errorf("expected empty HeadRepoName for null head repo, got %q", pr.HeadRepoName)
	}
	if pr.CloneURL != "" {
		t.Errorf("expected empty CloneURL for null head repo, got %q", pr.CloneURL)
	}
	if !pr.IsFork() {
		t.Errorf("expected IsFork() to be true for null head repo")
	}
}

func TestPRDetails_IsFork(t *testing.T) {
	tests := []struct {
		name string
		pr   *PRDetails
		want bool
	}{
		{
			name: "nil pr",
			pr:   nil,
			want: true,
		},
		{
			name: "missing head repo owner",
			pr: &PRDetails{
				Owner:         "upstream",
				HeadRepoOwner: "",
			},
			want: true,
		},
		{
			name: "same repo exact case",
			pr: &PRDetails{
				Owner:         "upstream",
				Repo:          "my-repo",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "my-repo",
			},
			want: false,
		},
		{
			name: "same repo different case",
			pr: &PRDetails{
				Owner:         "UpStream",
				Repo:          "My-Repo",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "my-repo",
			},
			want: false,
		},
		{
			name: "fork repo",
			pr: &PRDetails{
				Owner:         "upstream",
				Repo:          "repoA",
				HeadRepoOwner: "forker",
				HeadRepoName:  "repoA",
			},
			want: true,
		},
		{
			name: "same owner different repo",
			pr: &PRDetails{
				Owner:         "upstream",
				Repo:          "repoA",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "repoB",
			},
			want: true,
		},
		{
			name: "missing head repo name",
			pr: &PRDetails{
				Owner:         "upstream",
				Repo:          "repoA",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "",
			},
			want: true,
		},
		{
			name: "same owner same repo",
			pr: &PRDetails{
				Owner:         "upstream",
				Repo:          "repoA",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "repoA",
			},
			want: false,
		},
		{
			name: "same owner same repo different case",
			pr: &PRDetails{
				Owner:         "UpStream",
				Repo:          "RepoA",
				HeadRepoOwner: "upstream",
				HeadRepoName:  "repoa",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pr.IsFork(); got != tt.want {
				t.Errorf("IsFork() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanWriteRepository(t *testing.T) {
	for _, tc := range []struct {
		permission string
		allowed    bool
	}{
		{"admin", true}, {"maintain", true}, {"write", true},
		{"read", false}, {"triage", false}, {"none", false}, {"", false},
		{"guest", false}, {"billing", false}, {"unknown_custom", false},
	} {
		t.Run(tc.permission, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/org/repo/collaborators/member/permission" {
					t.Errorf("unexpected permission path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{"permission": tc.permission})
			}))
			defer server.Close()
			client, err := NewTestClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := client.CanWriteRepository(context.Background(), "org", "repo", "member")
			if err != nil || allowed != tc.allowed {
				t.Fatalf("allowed=%v err=%v, want %v", allowed, err, tc.allowed)
			}
		})
	}

	// API error status codes must all fail-closed (deny permission)
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusBadGateway,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, http.StatusText(status), status)
			}))
			defer server.Close()
			client, err := NewTestClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := client.CanWriteRepository(context.Background(), "org", "repo", "member")
			if err == nil || allowed {
				t.Fatalf("expected denied permission and error on %d, got allowed=%v err=%v", status, allowed, err)
			}
		})
	}

	// Malformed JSON response must fail closed
	t.Run("malformed JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{not valid json`))
		}))
		defer server.Close()
		client, err := NewTestClient(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		allowed, err := client.CanWriteRepository(context.Background(), "org", "repo", "member")
		if err == nil || allowed {
			t.Fatalf("expected error and denial on malformed JSON, got allowed=%v err=%v", allowed, err)
		}
	})

	// Empty and whitespace username must be denied without API call
	for _, emptyUser := range []string{"", " ", "\t\n"} {
		t.Run("empty_user_"+emptyUser, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				http.NotFound(w, r)
			}))
			defer server.Close()
			client, err := NewTestClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := client.CanWriteRepository(context.Background(), "org", "repo", emptyUser)
			if err != nil || allowed || called {
				t.Fatalf("expected empty/whitespace user denied without API call, got allowed=%v err=%v called=%v", allowed, err, called)
			}
		})
	}
}

func TestClient_GetRepoID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/my-org/my-repo" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   654321,
				"name": "my-repo",
			})
			return
		}
		if r.URL.Path == "/repos/my-org/zero-id" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   0,
				"name": "zero-id",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Success case
	repoID, err := client.GetRepoID(ctx, "my-org", "my-repo")
	if err != nil {
		t.Fatalf("unexpected error from GetRepoID: %v", err)
	}
	if repoID != 654321 {
		t.Errorf("expected repoID 654321, got: %d", repoID)
	}

	// 2. 404 Not Found
	_, err = client.GetRepoID(ctx, "my-org", "missing-repo")
	if err == nil {
		t.Fatalf("expected error for missing repo, got nil")
	}

	// 3. Zero ID
	_, err = client.GetRepoID(ctx, "my-org", "zero-id")
	if err == nil || !errors.Is(err, ErrInvalidRepoID) {
		t.Errorf("expected ErrInvalidRepoID for zero ID repo, got: %v", err)
	}
}

// TestClient_FailedSourceAccessPermitsPATReview tests that when narrow source retrieval fails
// (or is unavailable because only a PAT is configured), ordinary PAT review operations
// (such as GetPR and GetComments) continue to succeed, and no execution credential fallback occurs.
func TestClient_FailedSourceAccessPermitsPATReview(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/pulls/10":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 10,
				"title":  "Ordinary PAT review PR",
				"base": map[string]any{
					"ref":  "main",
					"repo": map[string]any{"id": 111, "name": "repo", "owner": map[string]any{"login": "owner"}},
				},
				"head": map[string]any{
					"ref":  "feat",
					"sha":  "abcdef1234567890abcdef1234567890abcdef12",
					"repo": map[string]any{"id": 222, "name": "repo", "owner": map[string]any{"login": "owner"}},
				},
			})
			return
		case "/repos/owner/repo/issues/10/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "body": "first comment"},
			})
			return
		case "/repos/owner/repo/pulls/10/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := NewTestClientWithToken(ts.URL, "ghp_ordinary_pat_for_review_only")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Source retrieval credential request fails because PAT cannot be used for retrieval
	cred, err := client.CreateRetrievalCredential(ctx, "owner", "repo", 222)
	if err == nil || !errors.Is(err, ErrRetrievalAuthUnavailable) {
		t.Fatalf("expected ErrRetrievalAuthUnavailable, got: %v (cred: %+v)", err, cred)
	}

	// 2. But ordinary review API calls with the PAT continue to succeed without hindrance
	pr, err := client.GetPR(ctx, "owner", "repo", 10)
	if err != nil {
		t.Fatalf("GetPR failed unexpectedly: %v", err)
	}
	if pr.Title != "Ordinary PAT review PR" {
		t.Errorf("unexpected PR title: %q", pr.Title)
	}
	if pr.HeadRepoID != 222 {
		t.Errorf("expected HeadRepoID to be 222, got %d", pr.HeadRepoID)
	}

	comments, threads, err := client.GetComments(ctx, "owner", "repo", 10)
	if err != nil {
		t.Fatalf("GetComments failed unexpectedly: %v", err)
	}
	if len(comments) != 1 || len(threads) != 0 {
		t.Errorf("expected 1 comment and 0 threads, got %d comments and %d threads", len(comments), len(threads))
	}
}

func TestValidateCommitOID(t *testing.T) {
	valid40 := "0123456789abcdef0123456789abcdef01234567"
	valid64 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	if err := ValidateCommitOID(valid40); err != nil {
		t.Fatalf("expected valid 40-char OID, got error: %v", err)
	}
	if err := ValidateCommitOID(valid64); err != nil {
		t.Fatalf("expected valid 64-char OID, got error: %v", err)
	}

	invalidCases := []struct {
		name string
		oid  string
	}{
		{"empty", ""},
		{"short 7-char prefix", "0123456"},
		{"39 chars", "0123456789abcdef0123456789abcdef0123456"},
		{"41 chars", "0123456789abcdef0123456789abcdef012345678"},
		{"non-hex char", "0123456789abcdef0123456789abcdef0123456g"},
		{"uppercase hex", "0123456789ABCDEF0123456789ABCDEF01234567"},
		{"spaces", " 0123456789abcdef0123456789abcdef0123456"},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCommitOID(tc.oid); err == nil {
				t.Fatalf("expected error for %s (%q), got nil", tc.name, tc.oid)
			}
		})
	}
}

func TestGetDiffAtCommits(t *testing.T) {
	baseOID := "1111111111111111111111111111111111111111"
	headOID := "2222222222222222222222222222222222222222"

	var acceptHeader string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptHeader = r.Header.Get("Accept")
		expectedPath := fmt.Sprintf("/repos/owner/repo/compare/%s...%s", baseOID, headOID)
		if r.Method == http.MethodGet && r.URL.Path == expectedPath {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/file.go b/file.go\n+new line\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	ctx := context.Background()

	// 1. Success
	diff, err := client.GetDiffAtCommits(ctx, "owner", "repo", baseOID, headOID)
	if err != nil {
		t.Fatalf("expected GetDiffAtCommits to succeed, got: %v", err)
	}
	if !strings.Contains(diff, "+new line") {
		t.Errorf("expected diff to contain '+new line', got: %s", diff)
	}
	if !strings.Contains(acceptHeader, "diff") {
		t.Errorf("expected Accept header to contain diff media type, got: %s", acceptHeader)
	}

	// 2. Reject invalid base OID
	if _, err := client.GetDiffAtCommits(ctx, "owner", "repo", "short123", headOID); err == nil {
		t.Errorf("expected error for short base OID, got nil")
	}

	// 3. Reject invalid head OID
	if _, err := client.GetDiffAtCommits(ctx, "owner", "repo", baseOID, "short456"); err == nil {
		t.Errorf("expected error for short head OID, got nil")
	}

	// 4. Reject empty owner/repo
	if _, err := client.GetDiffAtCommits(ctx, "", "repo", baseOID, headOID); err == nil {
		t.Errorf("expected error for empty owner, got nil")
	}
}

func TestCommitFileAtExpectedHead(t *testing.T) {
	expectedOID := "2222222222222222222222222222222222222222"
	newCommitOID := "3333333333333333333333333333333333333333"

	t.Run("success", func(t *testing.T) {
		var receivedReq map[string]any

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
				json.NewDecoder(r.Body).Decode(&receivedReq)
				resp := map[string]any{
					"data": map[string]any{
						"createCommitOnBranch": map[string]any{
							"commit": map[string]any{
								"oid": newCommitOID,
								"url": "https://github.com/owner/repo/commit/" + newCommitOID,
							},
							"ref": map[string]any{
								"name": "refs/heads/feature-branch",
								"target": map[string]any{
									"oid": newCommitOID,
								},
							},
						},
					},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(resp)
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, err := NewTestClient(ts.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		ctx := context.Background()
		sha, err := client.CommitFileAtExpectedHead(ctx, "owner", "repo", "refs/heads/feature-branch", expectedOID, "CHANGELOG.md", "# Changelog\n", "docs: update changelog")
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if sha != newCommitOID {
			t.Errorf("expected commit SHA %s, got %s", newCommitOID, sha)
		}

		// Verify GraphQL mutation variables
		variables, ok := receivedReq["variables"].(map[string]any)
		if !ok {
			t.Fatalf("variables missing in request: %+v", receivedReq)
		}
		input, ok := variables["input"].(map[string]any)
		if !ok {
			t.Fatalf("input missing in variables: %+v", variables)
		}
		if input["expectedHeadOid"] != expectedOID {
			t.Errorf("expected expectedHeadOid %s, got %v", expectedOID, input["expectedHeadOid"])
		}
		branch := input["branch"].(map[string]any)
		if branch["branchName"] != "feature-branch" {
			t.Errorf("expected branchName 'feature-branch', got %v", branch["branchName"])
		}
	})

	t.Run("head moved GraphQL error mapped to HeadMovedError", func(t *testing.T) {
		actualOID := "4444444444444444444444444444444444444444"
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				resp := map[string]any{
					"errors": []map[string]any{
						{
							"message": fmt.Sprintf("Expected branch to point to %q but it points to %q", expectedOID, actualOID),
							"type":    "UNPROCESSABLE",
						},
					},
				}
				json.NewEncoder(w).Encode(resp)
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, err := NewTestClient(ts.URL)
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}

		ctx := context.Background()
		_, err = client.CommitFileAtExpectedHead(ctx, "owner", "repo", "feature-branch", expectedOID, "CHANGELOG.md", "# Changelog", "msg")
		if err == nil {
			t.Fatalf("expected HeadMovedError, got nil")
		}

		var headMovedErr *HeadMovedError
		if !errors.As(err, &headMovedErr) {
			t.Fatalf("expected error to be *HeadMovedError, got: %T (%v)", err, err)
		}
		if headMovedErr.ExpectedHeadOID != expectedOID {
			t.Errorf("expected ExpectedHeadOID %s, got %s", expectedOID, headMovedErr.ExpectedHeadOID)
		}
		if headMovedErr.ActualHeadOID != actualOID {
			t.Errorf("expected ActualHeadOID %s, got %s", actualOID, headMovedErr.ActualHeadOID)
		}
		if !errors.Is(err, ErrHeadMoved) {
			t.Errorf("expected errors.Is(err, ErrHeadMoved) to be true")
		}
	})

	t.Run("HTTP 200 with other GraphQL error never reports success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"errors": []map[string]any{
					{"message": "Resource not accessible by integration"},
				},
			}
			json.NewEncoder(w).Encode(resp)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.CommitFileAtExpectedHead(context.Background(), "owner", "repo", "main", expectedOID, "CHANGELOG.md", "c", "m")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "Resource not accessible") {
			t.Errorf("unexpected error text: %v", err)
		}
		if errors.Is(err, ErrHeadMoved) {
			t.Errorf("should not be HeadMovedError for generic GraphQL error")
		}
	})

	t.Run("HTTP status error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Unauthorized access", http.StatusUnauthorized)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.CommitFileAtExpectedHead(context.Background(), "owner", "repo", "main", expectedOID, "CHANGELOG.md", "c", "m")
		if err == nil {
			t.Fatalf("expected error for 401, got nil")
		}
		if !strings.Contains(err.Error(), "auth error (status 401)") {
			t.Errorf("unexpected error text: %v", err)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "not-json{}}")
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.CommitFileAtExpectedHead(context.Background(), "owner", "repo", "main", expectedOID, "CHANGELOG.md", "c", "m")
		if err == nil {
			t.Fatalf("expected error for malformed JSON, got nil")
		}
	})

	t.Run("missing commit OID in response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": ""},
						"ref":    map[string]any{"name": "refs/heads/main"},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.CommitFileAtExpectedHead(context.Background(), "owner", "repo", "main", expectedOID, "CHANGELOG.md", "c", "m")
		if err == nil {
			t.Fatalf("expected error for empty commit OID, got nil")
		}
	})
}

func TestServiceActor(t *testing.T) {
	t.Run("PAT authenticated user success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user" {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id":    4242,
					"login": "service-bot",
				})
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, err := NewTestClient(ts.URL)
		if err != nil {
			t.Fatalf("NewTestClient error: %v", err)
		}

		actor, err := client.ServiceActor(context.Background(), "owner", "repo")
		if err != nil {
			t.Fatalf("unexpected ServiceActor error: %v", err)
		}
		if actor.ID != 4242 || actor.Login != "service-bot" {
			t.Errorf("got actor %+v, want ID 4242, Login 'service-bot'", actor)
		}

		actorID, err := client.ServiceActorID(context.Background(), "owner", "repo")
		if err != nil || actorID != 4242 {
			t.Errorf("got ServiceActorID (%d, %v), want (4242, nil)", actorID, err)
		}
	})

	t.Run("PAT permission error 401/403", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.ServiceActor(context.Background(), "owner", "repo")
		if err == nil || !errors.Is(err, ErrServiceActorPermission) {
			t.Errorf("expected ErrServiceActorPermission, got: %v", err)
		}
	})

	t.Run("PAT malformed user response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id": 0, "login": ""}`))
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		_, err := client.ServiceActor(context.Background(), "owner", "repo")
		if err == nil || !errors.Is(err, ErrServiceActorMalformed) {
			t.Errorf("expected ErrServiceActorMalformed, got: %v", err)
		}
	})

	t.Run("App JWT authenticated bot identity success", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa.GenerateKey error: %v", err)
		}

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/app":
				json.NewEncoder(w).Encode(map[string]any{
					"id":   1001,
					"slug": "pr-bot-app",
					"name": "PR Bot App",
				})
			case "/users/pr-bot-app[bot]":
				json.NewEncoder(w).Encode(map[string]any{
					"id":    8899,
					"login": "pr-bot-app[bot]",
					"type":  "Bot",
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer ts.Close()

		client, err := NewTestAppClient(ts.URL, 1001, key)
		if err != nil {
			t.Fatalf("NewTestAppClient error: %v", err)
		}

		actor, err := client.ServiceActor(context.Background(), "owner", "repo")
		if err != nil {
			t.Fatalf("unexpected ServiceActor error for App client: %v", err)
		}
		if actor.ID != 8899 || actor.Login != "pr-bot-app[bot]" {
			t.Errorf("got actor %+v, want ID 8899, Login 'pr-bot-app[bot]'", actor)
		}
	})
}

func TestOwnedCommentReconciliation(t *testing.T) {
	serviceActor := &ServiceActor{ID: 5555, Login: "my-service-bot"}
	marker := "<!-- pr-review-output:job-0001 -->"
	bodyText := "## Review Output\nAll good!\n\n" + marker
	h := sha256.Sum256([]byte(bodyText))
	validDigest := hex.EncodeToString(h[:])

	t.Run("matching service actor and marker and digest finds comment", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/10/comments") {
				w.Header().Set("Content-Type", "application/json")
				comments := []map[string]any{
					{
						"id":   101,
						"body": "User comment mentioning something",
						"user": map[string]any{"id": 9999, "login": "contributor"},
					},
					{
						"id":   102,
						"body": bodyText,
						"user": map[string]any{"id": 5555, "login": "my-service-bot"},
					},
				}
				json.NewEncoder(w).Encode(comments)
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		match, err := client.FindOwnedComment(context.Background(), FindOwnedCommentOptions{
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   10,
			Marker:     marker,
			BodyDigest: validDigest,
			Actor:      serviceActor,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !match.Found || match.CommentID != 102 || match.Uncertain {
			t.Errorf("got match %+v, want Found:true, CommentID:102, Uncertain:false", match)
		}
	})

	t.Run("forged marker in ordinary user comment is ignored (T-02-05)", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/10/comments") {
				w.Header().Set("Content-Type", "application/json")
				comments := []map[string]any{
					{
						"id":   201,
						"body": bodyText, // Copied marker and body by ordinary user!
						"user": map[string]any{"id": 7777, "login": "attacker"},
					},
				}
				json.NewEncoder(w).Encode(comments)
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		match, err := client.FindOwnedComment(context.Background(), FindOwnedCommentOptions{
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   10,
			Marker:     marker,
			BodyDigest: validDigest,
			Actor:      serviceActor,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if match.Found {
			t.Errorf("forged marker from ordinary user must not be accepted as owned output!")
		}
		if match.Uncertain {
			t.Errorf("complete scan with no owned comments must return Uncertain: false")
		}
	})

	t.Run("marker with mismatched body digest is ignored", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/repos/owner/repo/issues/10/comments") {
				w.Header().Set("Content-Type", "application/json")
				comments := []map[string]any{
					{
						"id":   301,
						"body": "Modified content " + marker,
						"user": map[string]any{"id": 5555, "login": "my-service-bot"},
					},
				}
				json.NewEncoder(w).Encode(comments)
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		match, err := client.FindOwnedComment(context.Background(), FindOwnedCommentOptions{
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   10,
			Marker:     marker,
			BodyDigest: validDigest, // won't match "Modified content "
			Actor:      serviceActor,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if match.Found {
			t.Errorf("expected match.Found to be false for mismatched body digest")
		}
	})

	t.Run("incomplete or failed scan yields uncertain, never absent", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()

		client, _ := NewTestClient(ts.URL)
		match, err := client.FindOwnedComment(context.Background(), FindOwnedCommentOptions{
			Owner:      "owner",
			Repo:       "repo",
			PRNumber:   10,
			Marker:     marker,
			BodyDigest: validDigest,
			Actor:      serviceActor,
		})
		if err == nil {
			t.Fatalf("expected error from 500 response")
		}
		if !match.Uncertain {
			t.Errorf("failed scan MUST return Uncertain: true, got false")
		}
		if match.Found {
			t.Errorf("failed scan must not claim comment found")
		}
	})
}
