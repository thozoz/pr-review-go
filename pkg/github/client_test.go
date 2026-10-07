package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
