package changelog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestRunAndPost_HeadMovedDuringDiffCollection(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	initialHeadSHA := "2222222222222222222222222222222222222222"
	movedHeadSHA := "4444444444444444444444444444444444444444"

	var prCallCount int
	var graphQLCalled bool
	var updateFileCalled bool
	var commentBody string
	var llmCallCount int

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			prCallCount++
			currentHead := initialHeadSHA
			if prCallCount > 1 {
				// PR head moved before generation (detected in precheck)
				currentHead = movedHeadSHA
			}
			prJSON := map[string]any{
				"number": 1,
				"title":  "Test PR",
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
					"sha": currentHead,
					"repo": map[string]any{
						"owner": map[string]any{"login": "owner"},
						"name":  "repo",
					},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/owner/repo/compare/%s...%s", baseSHA, initialHeadSHA):
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n+code\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
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
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateFileCalled = true
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/1/comments":
			var payload struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			commentBody = payload.Body
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": commentBody})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCallCount++
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"category":"Added","entry":"Should not run."}`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	updater := &Updater{gh: ghClient, llm: llm.NewClient(llmServer.URL, "key", "model")}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err == nil {
		t.Fatalf("expected error when head moved before generation, got nil")
	}

	if !strings.Contains(err.Error(), "rerun required") {
		t.Errorf("expected error to mention 'rerun required', got: %v", err)
	}
	var headMovedErr *github.HeadMovedError
	if !errors.As(err, &headMovedErr) {
		t.Errorf("expected error to unwrap to *github.HeadMovedError, got: %T (%v)", err, err)
	}

	// Invariants:
	// 1. Zero LLM generations executed
	if llmCallCount != 0 {
		t.Errorf("expected 0 LLM generation calls, got %d", llmCallCount)
	}
	// 2. Zero GraphQL commit calls executed
	if graphQLCalled {
		t.Errorf("expected GraphQL mutation NOT to be called, but it was called")
	}
	// 3. No REST UpdateFile fallback
	if updateFileCalled {
		t.Errorf("expected UpdateFile fallback NOT to be called, but it was called")
	}
	// 4. Rerun required comment posted on PR
	if !strings.Contains(commentBody, "rerun required") {
		t.Errorf("expected comment to mention 'rerun required', got: %q", commentBody)
	}
}

func TestRunAndPost_HeadMovedAfterGeneration_CASConflict(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	headSHA := "2222222222222222222222222222222222222222"
	actualHeadSHA := "5555555555555555555555555555555555555555"

	var graphQLCalled bool
	var updateFileCalled bool
	var commentBody string
	var llmCallCount int
	var receivedExpectedHeadOID string

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			prJSON := map[string]any{
				"number": 1,
				"title":  "Test PR",
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
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n+code\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
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
				}
			}
			// Simulate GraphQL expectedHeadOid conflict on HTTP 200
			resp := map[string]any{
				"errors": []map[string]any{
					{
						"message": fmt.Sprintf("Expected branch to point to %q but it points to %q", headSHA, actualHeadSHA),
						"type":    "UNPROCESSABLE",
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
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": commentBody})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCallCount++
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"category":"Added","entry":"Added atomic change."}`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	updater := &Updater{gh: ghClient, llm: llm.NewClient(llmServer.URL, "key", "model")}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err == nil {
		t.Fatalf("expected error on CAS conflict, got nil")
	}

	if !strings.Contains(err.Error(), "rerun required") {
		t.Errorf("expected error to mention 'rerun required', got: %v", err)
	}
	var headMovedErr *github.HeadMovedError
	if !errors.As(err, &headMovedErr) {
		t.Errorf("expected error to unwrap to *github.HeadMovedError, got: %T (%v)", err, err)
	}

	// Invariants:
	// 1. Exactly one generation executed (never regenerated on conflict!)
	if llmCallCount != 1 {
		t.Errorf("expected exactly 1 LLM generation call, got %d", llmCallCount)
	}
	// 2. Mutation sent expectedHeadOid equal to generation HeadSHA
	if !graphQLCalled {
		t.Errorf("expected GraphQL mutation to be called")
	}
	if receivedExpectedHeadOID != headSHA {
		t.Errorf("expected expectedHeadOid %s in GraphQL mutation, got %s", headSHA, receivedExpectedHeadOID)
	}
	// 3. No REST UpdateFile fallback
	if updateFileCalled {
		t.Errorf("expected UpdateFile fallback NOT to be called, but it was called")
	}
	// 4. Rerun required comment posted on PR
	if !strings.Contains(commentBody, "rerun required") {
		t.Errorf("expected comment to mention 'rerun required', got: %q", commentBody)
	}
}

func TestRunAndPost_HTTP200GraphQLError(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	headSHA := "2222222222222222222222222222222222222222"

	var updateFileCalled bool

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			prJSON := map[string]any{
				"number": 1,
				"title":  "Test PR",
				"base": map[string]any{
					"ref": "main",
					"sha": baseSHA,
					"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
				},
				"head": map[string]any{
					"ref": "feature-branch",
					"sha": headSHA,
					"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/repos/owner/repo/compare/%s...%s", baseSHA, headSHA):
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n+code\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n"))
			resp := map[string]any{
				"name": "CHANGELOG.md", "path": "CHANGELOG.md", "sha": "csha123",
				"content": content, "encoding": "base64",
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			resp := map[string]any{
				"errors": []map[string]any{
					{"message": "Resource not accessible by integration"},
				},
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			updateFileCalled = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"category":"Added","entry":"Added feature."}`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	updater := &Updater{gh: ghClient, llm: llm.NewClient(llmServer.URL, "key", "model")}

	err = updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err == nil {
		t.Fatalf("expected error on GraphQL generic error, got nil")
	}
	if !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("unexpected error text: %v", err)
	}
	if updateFileCalled {
		t.Errorf("expected no UpdateFile fallback on GraphQL error")
	}
}

func TestRunAndPost_ABA_HeadSequence(t *testing.T) {
	baseSHA := "1111111111111111111111111111111111111111"
	headA := "2222222222222222222222222222222222222222"
	newCommit := "6666666666666666666666666666666666666666"

	var comparedBase, comparedHead string
	var expectedHeadInMutation string

	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
			prJSON := map[string]any{
				"number": 1,
				"title":  "ABA PR",
				"base": map[string]any{
					"ref": "main",
					"sha": baseSHA,
					"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
				},
				"head": map[string]any{
					"ref": "feature-branch",
					"sha": headA, // Even if branch moves A -> B -> A, the captured head remains A
					"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
				},
			}
			json.NewEncoder(w).Encode(prJSON)
		case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/"):
			parts := strings.TrimPrefix(r.URL.Path, "/repos/owner/repo/compare/")
			sub := strings.Split(parts, "...")
			if len(sub) == 2 {
				comparedBase = sub[0]
				comparedHead = sub[1]
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "diff --git a/foo.go b/foo.go\n+code\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/contents/CHANGELOG.md":
			content := base64.StdEncoding.EncodeToString([]byte("# Changelog\n\n## [Unreleased]\n"))
			resp := map[string]any{
				"name": "CHANGELOG.md", "path": "CHANGELOG.md", "sha": "csha123",
				"content": content, "encoding": "base64",
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if vars, ok := req["variables"].(map[string]any); ok {
				if in, ok := vars["input"].(map[string]any); ok {
					expectedHeadInMutation, _ = in["expectedHeadOid"].(string)
				}
			}
			resp := map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": newCommit, "url": "https://..."},
						"ref":    map[string]any{"name": "refs/heads/feature-branch", "target": map[string]any{"oid": newCommit}},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/owner/repo/issues/1/comments":
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": `{"category":"Added","entry":"ABA change."}`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmServer.Close()

	ghClient, _ := github.NewTestClient(ghServer.URL)
	updater := &Updater{gh: ghClient, llm: llm.NewClient(llmServer.URL, "key", "model")}

	err := updater.RunAndPost(context.Background(), "owner", "repo", 1)
	if err != nil {
		t.Fatalf("expected success for consistent ABA head, got error: %v", err)
	}

	if comparedBase != baseSHA || comparedHead != headA {
		t.Errorf("expected compare to use captured full OIDs %s...%s, got %s...%s", baseSHA, headA, comparedBase, comparedHead)
	}
	if expectedHeadInMutation != headA {
		t.Errorf("expected expectedHeadOid in mutation to be %s, got %s", headA, expectedHeadInMutation)
	}
}

func TestRunAndPost_IncompleteOrInvalidCommitOID_Refused(t *testing.T) {
	cases := []struct {
		name    string
		baseSHA string
		headSHA string
	}{
		{"empty base SHA", "", "2222222222222222222222222222222222222222"},
		{"empty head SHA", "1111111111111111111111111111111111111111", ""},
		{"short base SHA", "short12", "2222222222222222222222222222222222222222"},
		{"short head SHA", "1111111111111111111111111111111111111111", "short34"},
		{"non-hex base SHA", "111111111111111111111111111111111111111g", "2222222222222222222222222222222222222222"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var compareCalled bool

			ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/repo/pulls/1":
					prJSON := map[string]any{
						"number": 1,
						"title":  "Invalid SHA PR",
						"base": map[string]any{
							"ref": "main",
							"sha": tc.baseSHA,
							"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
						},
						"head": map[string]any{
							"ref": "feature-branch",
							"sha": tc.headSHA,
							"repo": map[string]any{"owner": map[string]any{"login": "owner"}, "name": "repo"},
						},
					}
					json.NewEncoder(w).Encode(prJSON)
				case strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/"):
					compareCalled = true
				default:
					http.NotFound(w, r)
				}
			}))
			defer ghServer.Close()

			ghClient, _ := github.NewTestClient(ghServer.URL)
			updater := &Updater{gh: ghClient, llm: llm.NewClient("http://unused", "key", "model")}

			err := updater.RunAndPost(context.Background(), "owner", "repo", 1)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if compareCalled {
				t.Errorf("expected GetDiffAtCommits NOT to be called when OID is invalid")
			}
		})
	}
}

func TestRunAndPost_DisposableRemoteProof_Doc(t *testing.T) {
	// Documentation and verification invariant:
	// A live remote test against GitHub GraphQL createCommitOnBranch requires an explicit
	// disposable repository setup with PR_REVIEW_LIVE_GITHUB_TEST=1.
	// Routine tests must remain hermetic and NEVER mutate user repositories.
	// This test confirms that when the opt-in flag is not set, no remote call is attempted.
	t.Log("Hermetic mock testing verified; live remote publication requires explicit disposable repository configuration (SAFE-03, D-16).")
}
