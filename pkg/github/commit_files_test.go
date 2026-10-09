package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/retry"
)

// commitSuccessServer returns an httptest server that answers the GraphQL
// createCommitOnBranch mutation with newCommitOID and counts mutation requests.
func commitSuccessServer(t *testing.T, newCommitOID string, requests *atomic.Int32, onInput func(input map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
			requests.Add(1)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if onInput != nil {
				vars, _ := req["variables"].(map[string]any)
				in, _ := vars["input"].(map[string]any)
				onInput(in)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid": newCommitOID,
							"url": "https://github.com/owner/repo/commit/" + newCommitOID,
						},
						"ref": map[string]any{
							"name":   "refs/heads/feature-branch",
							"target": map[string]any{"oid": newCommitOID},
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
}

// TestCommitFilesAtExpectedHead_TwoFilesSingleMutation asserts D-22: one comment
// asking two fixes produces exactly one bot commit via exactly one HTTP mutation.
func TestCommitFilesAtExpectedHead_TwoFilesSingleMutation(t *testing.T) {
	expectedOID := "2222222222222222222222222222222222222222"
	newCommitOID := "3333333333333333333333333333333333333333"
	var requests atomic.Int32
	var gotInput map[string]any

	ts := commitSuccessServer(t, newCommitOID, &requests, func(in map[string]any) { gotInput = in })
	defer ts.Close()

	client, err := NewTestClient(ts.URL)
	if err != nil {
		t.Fatalf("NewTestClient: %v", err)
	}

	files := map[string]string{
		"pkg/limiter/limiter.go": "package limiter\n",
		"README.md":              "# updated\n",
	}
	sha, err := client.CommitFilesAtExpectedHead(context.Background(), "owner", "repo", "feature-branch", expectedOID, "pr-review: add rate limiter \"x\" (abc1234)", files, nil)
	if err != nil {
		t.Fatalf("CommitFilesAtExpectedHead: %v", err)
	}
	if sha != newCommitOID {
		t.Errorf("expected commit SHA %s, got %s", newCommitOID, sha)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("expected exactly 1 mutation request for the two-file case, got %d", got)
	}
	if gotInput["expectedHeadOid"] != expectedOID {
		t.Errorf("expected expectedHeadOid %s, got %v", expectedOID, gotInput["expectedHeadOid"])
	}
	branch, _ := gotInput["branch"].(map[string]any)
	if branch["branchName"] != "feature-branch" {
		t.Errorf("expected branchName 'feature-branch', got %v", branch["branchName"])
	}
	fc, _ := gotInput["fileChanges"].(map[string]any)
	adds, _ := fc["additions"].([]any)
	if len(adds) != 2 {
		t.Fatalf("expected 2 additions, got %d", len(adds))
	}
	seen := map[string]string{}
	for _, a := range adds {
		m, _ := a.(map[string]any)
		path, _ := m["path"].(string)
		enc, _ := m["contents"].(string)
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("addition %q contents not base64: %v", path, err)
		}
		seen[path] = string(raw)
	}
	for p, want := range files {
		if seen[p] != want {
			t.Errorf("addition %q = %q, want %q", p, seen[p], want)
		}
	}
	if _, ok := fc["deletions"]; ok {
		t.Errorf("deletions key should be absent when no deletions requested, got %v", fc["deletions"])
	}
}

// TestCommitFilesAtExpectedHead_Deletions asserts the optional deletions array shape.
func TestCommitFilesAtExpectedHead_Deletions(t *testing.T) {
	expectedOID := "2222222222222222222222222222222222222222"
	newCommitOID := "3333333333333333333333333333333333333333"
	var requests atomic.Int32
	var gotInput map[string]any

	ts := commitSuccessServer(t, newCommitOID, &requests, func(in map[string]any) { gotInput = in })
	defer ts.Close()

	client, _ := NewTestClient(ts.URL)
	_, err := client.CommitFilesAtExpectedHead(context.Background(), "owner", "repo", "refs/heads/feature-branch", expectedOID, "headline", map[string]string{"a.go": "x"}, []string{"old.go", "/stale.go"})
	if err != nil {
		t.Fatalf("CommitFilesAtExpectedHead: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("expected exactly 1 mutation request, got %d", got)
	}
	fc, _ := gotInput["fileChanges"].(map[string]any)
	dels, _ := fc["deletions"].([]any)
	if len(dels) != 2 {
		t.Fatalf("expected 2 deletions, got %v", fc["deletions"])
	}
	paths := map[string]bool{}
	for _, d := range dels {
		m, _ := d.(map[string]any)
		p, _ := m["path"].(string)
		paths[p] = true
	}
	if !paths["old.go"] || !paths["stale.go"] {
		t.Errorf("deletion paths not normalized as expected: %v", paths)
	}
	branch, _ := gotInput["branch"].(map[string]any)
	if branch["branchName"] != "feature-branch" {
		t.Errorf("refs/heads/ prefix should be stripped, got %v", branch["branchName"])
	}
}

// TestCommitFilesAtExpectedHead_HeadMoved asserts stale-head CAS mapping (D-31/D-32:
// typed error with parsed 40-hex ActualHeadOID, never retried, never REST-fallback).
func TestCommitFilesAtExpectedHead_HeadMoved(t *testing.T) {
	expectedOID := "2222222222222222222222222222222222222222"
	actualOID := "4444444444444444444444444444444444444444"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{
				{"message": fmt.Sprintf("Expected branch to point to %q but it points to %q", expectedOID, actualOID), "type": "UNPROCESSABLE"},
			},
		})
	}))
	defer ts.Close()

	client, _ := NewTestClient(ts.URL)
	_, err := client.CommitFilesAtExpectedHead(context.Background(), "owner", "repo", "feature-branch", expectedOID, "headline", map[string]string{"a.go": "x"}, nil)
	if err == nil {
		t.Fatalf("expected HeadMovedError, got nil")
	}
	var hme *HeadMovedError
	if !errors.As(err, &hme) {
		t.Fatalf("expected *HeadMovedError, got %T (%v)", err, err)
	}
	if hme.ExpectedHeadOID != expectedOID {
		t.Errorf("ExpectedHeadOID = %s, want %s", hme.ExpectedHeadOID, expectedOID)
	}
	if hme.ActualHeadOID != actualOID {
		t.Errorf("ActualHeadOID = %s, want %s (40-hex parsed)", hme.ActualHeadOID, actualOID)
	}
	if !errors.Is(err, ErrHeadMoved) {
		t.Errorf("errors.Is(err, ErrHeadMoved) = false")
	}
}

// TestCommitFilesAtExpectedHead_Validation pins the fail-closed preamble: the
// function never substitutes a branch and rejects empty/invalid inputs.
func TestCommitFilesAtExpectedHead_Validation(t *testing.T) {
	goodOID := "2222222222222222222222222222222222222222"
	files := map[string]string{"a.go": "x"}
	cases := []struct {
		name       string
		owner      string
		repo       string
		branch     string
		oid        string
		headline   string
		files      map[string]string
		deletions  []string
	}{
		{"empty owner", "", "repo", "b", goodOID, "h", files, nil},
		{"empty repo", "owner", "", "b", goodOID, "h", files, nil},
		{"empty branch never defaults", "owner", "repo", "  ", goodOID, "h", files, nil},
		{"empty headline", "owner", "repo", "b", goodOID, "  ", files, nil},
		{"bad expectedHeadOID", "owner", "repo", "b", "abc", "h", files, nil},
		{"no files and no deletions", "owner", "repo", "b", goodOID, "h", nil, nil},
		{"empty file path", "owner", "repo", "b", goodOID, "h", map[string]string{"  ": "x"}, nil},
		{"empty deletion path", "owner", "repo", "b", goodOID, "h", nil, []string{"  "}},
	}
	client := NewClient("")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.CommitFilesAtExpectedHead(context.Background(), tc.owner, tc.repo, tc.branch, tc.oid, tc.headline, tc.files, tc.deletions)
			if err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
}

// TestCommitFilesAtExpectedHead_403Permanent asserts D-25 refusal semantics: a
// branch-protection/permission 403 classifies permanent, never uncertain, so the
// executor comments why instead of retrying or bypassing.
func TestCommitFilesAtExpectedHead_403Permanent(t *testing.T) {
	expectedOID := "2222222222222222222222222222222222222222"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "Resource protected by branch protection"}`,
			http.StatusForbidden)
	}))
	defer ts.Close()

	client, _ := NewTestClient(ts.URL)
	_, err := client.CommitFilesAtExpectedHead(context.Background(), "owner", "repo", "feature-branch", expectedOID, "headline", map[string]string{"a.go": "x"}, nil)
	if err == nil {
		t.Fatalf("expected 403 error, got nil")
	}
	if !strings.Contains(err.Error(), "auth error (status 403)") {
		t.Errorf("expected explanatory 403 error, got: %v", err)
	}
	if got := ClassifyWriteError(err); got != retry.ClassPermanent {
		t.Errorf("ClassifyWriteError(403) = %q, want permanent", got)
	}
	if IsUncertainWriteError(err) {
		t.Errorf("IsUncertainWriteError(403) = true, want false")
	}
}

// TestSanitizeCommitHeadline pins the D-35 headline contract: single line,
// 140-char cap, quoted original (TR preserved), caller English summary, 7-char SHA.
func TestSanitizeCommitHeadline(t *testing.T) {
	cases := []struct {
		name     string
		summary  string
		original string
		sha      string
		contains []string
	}{
		{
			name:     "summary plus quoted TR original",
			summary:  "Add rate limiter",
			original: "rate limiter ekle",
			sha:      "abc1234",
			contains: []string{"pr-review:", "Add rate limiter", `"rate limiter ekle"`, "(abc1234)"},
		},
		{
			name:     "empty summary falls back to TR-quote-only",
			summary:  "   ",
			original: "hatayı düzelt ve gönder",
			sha:      "def5678",
			contains: []string{"pr-review:", `"hatayı düzelt ve gönder"`, "(def5678)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeCommitHeadline(tc.summary, tc.original, tc.sha)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("headline %q missing %q", got, want)
				}
			}
			if strings.Contains(got, "\n") {
				t.Errorf("headline must be single-line, got %q", got)
			}
			if len([]rune(got)) > 140 {
				t.Errorf("headline exceeds 140 chars (%d): %q", len([]rune(got)), got)
			}
		})
	}

	t.Run("whitespace and control chars collapsed", func(t *testing.T) {
		got := SanitizeCommitHeadline("fix  bug\nnow", "düzelt\x00\n\tgönder", "abc1234")
		if strings.ContainsAny(got, "\n\t\x00") {
			t.Errorf("control chars/newlines must be stripped: %q", got)
		}
		if strings.Contains(got, "  ") {
			t.Errorf("whitespace runs must collapse: %q", got)
		}
	})

	t.Run("long inputs capped at 140 with original preserved", func(t *testing.T) {
		long := strings.Repeat("word ", 60)
		got := SanitizeCommitHeadline(long, "kısa komut", "abc1234")
		if len([]rune(got)) > 140 {
			t.Errorf("headline exceeds 140 chars (%d)", len([]rune(got)))
		}
		if !strings.Contains(got, `"kısa komut"`) {
			t.Errorf("quoted original must survive truncation: %q", got)
		}
	})

	t.Run("original truncated to 60 chars at word boundary", func(t *testing.T) {
		original := "bu çok uzun bir talimat metnidir ve altmış karakteri aşar kesinlikle"
		got := SanitizeCommitHeadline("", original, "abc1234")
		inner := got[strings.Index(got, `"`)+1 : strings.LastIndex(got, `"`)]
		if len([]rune(inner)) > 60 {
			t.Errorf("quoted original exceeds 60 chars: %q", inner)
		}
		if strings.HasSuffix(inner, " ") {
			t.Errorf("truncation must not leave trailing space: %q", inner)
		}
	})

	t.Run("invalid SHA yields placeholder", func(t *testing.T) {
		for _, bad := range []string{"", "xyz", "abc123456789", "zzzzzzz"} {
			got := SanitizeCommitHeadline("s", "o", bad)
			if !strings.Contains(got, "(0000000)") {
				t.Errorf("bad sha %q should yield placeholder, got %q", bad, got)
			}
		}
	})
}

// TestPushPathHasNoForceConcept enforces T-05-04 structurally: the GraphQL
// createCommitOnBranch mutation has no force parameter by API design, and no
// UpdateRef force path may exist in the push transport.
func TestPushPathHasNoForceConcept(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	clientGo := filepath.Join(filepath.Dir(thisFile), "client.go")
	src, err := os.ReadFile(clientGo)
	if err != nil {
		t.Fatalf("read client.go: %v", err)
	}
	for _, banned := range []string{"UpdateRef", `"force"`, "Force: true", "force-push"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("push transport must not contain %q (non-force guarantee is structural)", banned)
		}
	}
}
