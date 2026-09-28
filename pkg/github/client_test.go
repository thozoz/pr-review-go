package github

import (
	"context"
	"encoding/json"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := NewTestClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := client.CanWriteRepository(context.Background(), "org", "repo", "member")
	if err == nil || allowed {
		t.Fatalf("expected denied permission on API error, got allowed=%v err=%v", allowed, err)
	}
	allowed, err = client.CanWriteRepository(context.Background(), "org", "repo", "")
	if err != nil || allowed {
		t.Fatalf("expected empty username denied without API call, got allowed=%v err=%v", allowed, err)
	}
}
