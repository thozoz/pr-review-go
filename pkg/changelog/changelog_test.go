package changelog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

func TestChangesChangelog(t *testing.T) {
	if !ChangesChangelog("diff --git a/CHANGELOG.md b/CHANGELOG.md\n") {
		t.Fatal("expected changelog change")
	}
	if ChangesChangelog("diff --git a/README.md b/README.md\n") {
		t.Fatal("unexpected changelog change")
	}
}

func TestInsertUnreleasedEntryExistingCategory(t *testing.T) {
	content := "# Changelog\n\n## [Unreleased]\n\n### Fixed\n- Old fix\n\n## [1.0.0]\n"
	updated, err := InsertUnreleasedEntry(content, "Fixed", "Prevent timeout.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "### Fixed\n- Prevent timeout.\n- Old fix") {
		t.Fatalf("entry not inserted: %s", updated)
	}
}

func TestInsertUnreleasedEntryCreatesCategory(t *testing.T) {
	content := "# Changelog\n\n## [Unreleased]\n\n## [1.0.0]\n"
	updated, err := InsertUnreleasedEntry(content, "Added", "Expose new endpoint.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "## [Unreleased]\n\n### Added\n\n- Expose new endpoint.") {
		t.Fatalf("category not inserted: %s", updated)
	}
}

func TestRunAndPost_ForkMismatchRegression(t *testing.T) {
	var diffCalled bool
	var contentCalled bool
	var updateCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				diffCalled = true
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "diff --git a/foo.go b/foo.go\nindex 0000000..1111111 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -0,0 +1 @@\n+// new code\n")
				return
			}
			// PR from fork: head.repo.owner is "fork-user", head.ref is "main" (matches base branch "main")
			prJSON := map[string]any{
				"number": 1,
				"title":  "Fork PR with main branch",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "main",
					"sha": "forksha123",
					"repo": map[string]any{
						"owner": map[string]any{"login": "fork-user"},
						"name":  "repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n"))
			resp := map[string]any{
				"name":     "CHANGELOG.md",
				"path":     "CHANGELOG.md",
				"sha":      "oldsha123",
				"content":  content,
				"encoding": "base64",
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateCalled = true
			resp := map[string]any{
				"commit": map[string]any{
					"sha": "newcommitsha456",
				},
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/1/comments":
			resp := map[string]any{"id": 1, "body": "comment"}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"category":"Added","entry":"Added fork feature."}`,
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create test github client: %v", err)
	}
	llmClient := llm.NewClient(llmServer.URL, "test-key", "test-model")
	updater := &Updater{gh: ghClient, llm: llmClient}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)

	if err == nil {
		t.Errorf("expected RunAndPost to refuse changelog update on fork PR, got nil error")
	}
	if diffCalled {
		t.Errorf("expected GetRawDiff NOT to be called for fork PR, but it was called")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called for fork PR, but it was called")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called for fork PR, but base branch was updated")
	}
}

func TestRunAndPost_SameRepoHeadSHA(t *testing.T) {
	var requestedRef string
	var updatedBranch string

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "diff --git a/foo.go b/foo.go\nindex 0000000..1111111 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -0,0 +1 @@\n+// new code\n")
				return
			}
			// Same-repo PR: head.repo.owner matches base repo owner
			prJSON := map[string]any{
				"number": 1,
				"title":  "Same repo PR",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "feature-branch",
					"sha": "immutablesha999",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			requestedRef = r.URL.Query().Get("ref")
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n"))
			resp := map[string]any{
				"name":     "CHANGELOG.md",
				"path":     "CHANGELOG.md",
				"sha":      "oldsha123",
				"content":  content,
				"encoding": "base64",
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			var payload struct {
				Branch string `json:"branch"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			updatedBranch = payload.Branch
			resp := map[string]any{
				"commit": map[string]any{
					"sha": "newcommitsha777",
				},
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/1/comments":
			resp := map[string]any{"id": 1, "body": "comment"}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"category":"Added","entry":"Added feature on branch."}`,
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create test github client: %v", err)
	}
	llmClient := llm.NewClient(llmServer.URL, "test-key", "test-model")
	updater := &Updater{gh: ghClient, llm: llmClient}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("expected RunAndPost to succeed for same-repo PR, got error: %v", err)
	}

	if requestedRef != "immutablesha999" {
		t.Errorf("expected GetFileContent to use immutable HeadSHA 'immutablesha999', got %q", requestedRef)
	}
	if updatedBranch != "feature-branch" {
		t.Errorf("expected UpdateFile to keep branch 'feature-branch', got %q", updatedBranch)
	}
}

func TestRunAndPost_NullHeadRepo(t *testing.T) {
	var diffCalled bool
	var contentCalled bool
	var updateCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				diffCalled = true
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n")
				return
			}
			// Head repo is null (e.g., fork repo was deleted)
			prJSON := map[string]any{
				"number": 1,
				"title":  "PR with deleted fork repo",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref":  "main",
					"sha":  "deletedrepo123",
					"repo": nil,
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateCalled = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create test github client: %v", err)
	}
	updater := &Updater{gh: ghClient, llm: llm.NewClient("http://unused", "key", "model")}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err == nil {
		t.Errorf("expected RunAndPost to fail safely for null head repo, got nil error")
	}
	if diffCalled {
		t.Errorf("expected GetRawDiff NOT to be called when head repo is null")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called when head repo is null")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called when head repo is null")
	}
}

func TestRunAndPost_SameOwnerDifferentRepo(t *testing.T) {
	var diffCalled bool
	var contentCalled bool
	var updateCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				diffCalled = true
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n")
				return
			}
			// Same owner, different repo name
			prJSON := map[string]any{
				"number": 1,
				"title":  "PR from distinct repo under same owner",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "main",
					"sha": "distinctsha123",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "other-repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateCalled = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create test github client: %v", err)
	}
	updater := &Updater{gh: ghClient, llm: llm.NewClient("http://unused", "key", "model")}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err == nil {
		t.Errorf("expected RunAndPost to refuse changelog update when repo name differs, got nil error")
	}
	if diffCalled {
		t.Errorf("expected GetRawDiff NOT to be called when repo name differs")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called when repo name differs")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called when repo name differs")
	}
}
