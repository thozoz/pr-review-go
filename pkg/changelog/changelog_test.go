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
	var compareCalled bool
	var contentCalled bool
	var updateCalled bool
	var graphQLCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			// PR from fork: head.repo.owner is "fork-user", head.ref is "main" (matches base branch "main")
			prJSON := map[string]any{
				"number": 1,
				"title":  "Fork PR with main branch",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"sha": "1111111111111111111111111111111111111111",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "main",
					"sha": "2222222222222222222222222222222222222222",
					"repo": map[string]any{
						"owner": map[string]any{"login": "fork-user"},
						"name":  "repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/"):
			compareCalled = true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphQLCalled = true
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateCalled = true
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
	if compareCalled {
		t.Errorf("expected GetDiffAtCommits NOT to be called for fork PR, but it was called")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called for fork PR, but it was called")
	}
	if graphQLCalled {
		t.Errorf("expected GraphQL mutation NOT to be called for fork PR, but it was called")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called for fork PR, but base branch was updated")
	}
}

func TestRunAndPost_SameRepoHeadSHA(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	headSHA := "2222222222222222222222222222222222222222"
	newCommitSHA := "3333333333333333333333333333333333333333"

	var compareCalled bool
	var contentRequestedRef string
	var graphQLCalled bool
	var receivedExpectedHeadOID string
	var receivedBranch string
	var receivedPath string
	var updateFileCalled bool
	var commentBody string
	var llmCallCount int

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			prJSON := map[string]any{
				"number": 1,
				"title":  "Same repo PR",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"sha": baseSHA,
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "feature-branch",
					"sha": headSHA,
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/owner/repo/compare/%s...%s", baseSHA, headSHA):
			compareCalled = true
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/foo.go b/foo.go\nindex 0000000..1111111 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -0,0 +1 @@\n+// new code\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentRequestedRef = r.URL.Query().Get("ref")
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n"))
			resp := map[string]any{
				"name":     "CHANGELOG.md",
				"path":     "CHANGELOG.md",
				"sha":      "csha123",
				"content":  content,
				"encoding": "base64",
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphQLCalled = true
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if vars, ok := req["variables"].(map[string]any); ok {
				if in, ok := vars["input"].(map[string]any); ok {
					if exp, ok := in["expectedHeadOid"].(string); ok {
						receivedExpectedHeadOID = exp
					}
					if br, ok := in["branch"].(map[string]any); ok {
						if name, ok := br["branchName"].(string); ok {
							receivedBranch = name
						}
					}
					if fc, ok := in["fileChanges"].(map[string]any); ok {
						if adds, ok := fc["additions"].([]any); ok && len(adds) > 0 {
							if firstAdd, ok := adds[0].(map[string]any); ok {
								if p, ok := firstAdd["path"].(string); ok {
									receivedPath = p
								}
							}
						}
					}
				}
			}
			resp := map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid": newCommitSHA,
							"url": "https://github.com/owner/repo/commit/" + newCommitSHA,
						},
						"ref": map[string]any{
							"name": "refs/heads/feature-branch",
							"target": map[string]any{
								"oid": newCommitSHA,
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateFileCalled = true
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/1/comments":
			var payload struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			commentBody = payload.Body
			resp := map[string]any{"id": 1, "body": commentBody}
			json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCallCount++
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

	if !compareCalled {
		t.Errorf("expected GetDiffAtCommits to be called for immutable compare")
	}
	if contentRequestedRef != headSHA {
		t.Errorf("expected GetFileContent to use immutable HeadSHA %q, got %q", headSHA, contentRequestedRef)
	}
	if !graphQLCalled {
		t.Errorf("expected GraphQL createCommitOnBranch to be called")
	}
	if receivedExpectedHeadOID != headSHA {
		t.Errorf("expected GraphQL mutation to supply expectedHeadOid %q, got %q", headSHA, receivedExpectedHeadOID)
	}
	if receivedBranch != "feature-branch" {
		t.Errorf("expected GraphQL mutation branch to be 'feature-branch', got %q", receivedBranch)
	}
	if receivedPath != "CHANGELOG.md" {
		t.Errorf("expected GraphQL mutation file change path to be 'CHANGELOG.md', got %q", receivedPath)
	}
	if updateFileCalled {
		t.Errorf("expected REST UpdateFile NOT to be called, but it was called")
	}
	if llmCallCount != 1 {
		t.Errorf("expected exactly 1 LLM generation call, got %d", llmCallCount)
	}
	if !strings.Contains(commentBody, newCommitSHA) {
		t.Errorf("expected comment to mention commit SHA %s, got: %q", newCommitSHA, commentBody)
	}
}

func TestRunAndPost_NullHeadRepo(t *testing.T) {
	var compareCalled bool
	var contentCalled bool
	var updateCalled bool
	var graphQLCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			// Head repo is null (e.g., fork repo was deleted)
			prJSON := map[string]any{
				"number": 1,
				"title":  "PR with deleted fork repo",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"sha": "1111111111111111111111111111111111111111",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref":  "main",
					"sha":  "2222222222222222222222222222222222222222",
					"repo": nil,
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/"):
			compareCalled = true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphQLCalled = true
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
	if compareCalled {
		t.Errorf("expected GetDiffAtCommits NOT to be called when head repo is null")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called when head repo is null")
	}
	if graphQLCalled {
		t.Errorf("expected GraphQL mutation NOT to be called when head repo is null")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called when head repo is null")
	}
}

func TestRunAndPost_SameOwnerDifferentRepo(t *testing.T) {
	var compareCalled bool
	var contentCalled bool
	var updateCalled bool
	var graphQLCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			// Same owner, different repo name
			prJSON := map[string]any{
				"number": 1,
				"title":  "PR from distinct repo under same owner",
				"body":   "PR description",
				"base": map[string]any{
					"ref": "main",
					"sha": "1111111111111111111111111111111111111111",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
				"head": map[string]any{
					"ref": "main",
					"sha": "2222222222222222222222222222222222222222",
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "other-repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/"):
			compareCalled = true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			contentCalled = true
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphQLCalled = true
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
	if compareCalled {
		t.Errorf("expected GetDiffAtCommits NOT to be called when repo name differs")
	}
	if contentCalled {
		t.Errorf("expected GetFileContent NOT to be called when repo name differs")
	}
	if graphQLCalled {
		t.Errorf("expected GraphQL mutation NOT to be called when repo name differs")
	}
	if updateCalled {
		t.Errorf("expected UpdateFile NOT to be called when repo name differs")
	}
}
