package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSanitizeCommitHeadline(t *testing.T) {
	t.Run("normal summary and TR instruction", func(t *testing.T) {
		got := SanitizeCommitHeadline("Fix error handling", "hatayı düzelt ve\n\t  gönder", "a1b2c3d")
		want := `pr-review: Fix error handling "hatayı düzelt ve gönder" (a1b2c3d)`
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if len(got) > 140 {
			t.Errorf("length %d exceeds 140", len(got))
		}
	})

	t.Run("empty summary fallback", func(t *testing.T) {
		got := SanitizeCommitHeadline("   ", "rate limiter ekle", "abcdef1")
		want := `pr-review: "rate limiter ekle" (abcdef1)`
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("long instruction truncation and bounds", func(t *testing.T) {
		longInst := strings.Repeat("kelime ", 25)
		got := SanitizeCommitHeadline("Add big feature", longInst, "1234567")
		if len(got) > 140 {
			t.Errorf("length %d exceeds 140", len(got))
		}
		if !strings.Contains(got, "(1234567)") {
			t.Errorf("missing short sha in %q", got)
		}
	})
}

func TestCommitFilesAtExpectedHead(t *testing.T) {
	const (
		owner       = "test-org"
		repo        = "test-repo"
		branch      = "feature"
		expectedOID = "0123456789abcdef0123456789abcdef01234567"
		newOID      = "abcdef0123456789abcdef0123456789abcdef01"
	)

	t.Run("two files single mutation request", func(t *testing.T) {
		var reqCount int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&reqCount, 1)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("failed decoding request: %v", err)
			}
			variables := req["variables"].(map[string]any)
			input := variables["input"].(map[string]any)
			fileChanges := input["fileChanges"].(map[string]any)
			additions := fileChanges["additions"].([]any)
			if len(additions) != 2 {
				t.Errorf("expected 2 additions, got %d", len(additions))
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid": newOID,
							"url": "https://example.com/commit/" + newOID,
						},
						"ref": map[string]any{
							"name": "refs/heads/" + branch,
							"target": map[string]any{
								"oid": newOID,
							},
						},
					},
				},
			})
		}))
		defer srv.Close()

		client, err := NewTestClient(srv.URL)
		if err != nil {
			t.Fatal(err)
		}

		files := map[string]string{
			"file1.go": "package main",
			"file2.go": "package test",
		}
		oid, err := client.CommitFilesAtExpectedHead(context.Background(), owner, repo, branch, expectedOID, "test commit", files, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if oid != newOID {
			t.Errorf("got oid %s, want %s", oid, newOID)
		}
		if reqCount != 1 {
			t.Errorf("expected exactly 1 http request, got %d", reqCount)
		}
	})

	t.Run("head moved typed error with actual head", func(t *testing.T) {
		actualOID := "fedcba9876543210fedcba9876543210fedcba98"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errors": []map[string]any{
					{
						"message": "Expected update to refs/heads/" + branch + " at " + expectedOID + " but was " + actualOID,
					},
				},
			})
		}))
		defer srv.Close()

		client, err := NewTestClient(srv.URL)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.CommitFilesAtExpectedHead(context.Background(), owner, repo, branch, expectedOID, "test commit", map[string]string{"a.txt": "b"}, nil)
		if err == nil {
			t.Fatal("expected head moved error")
		}
		var headMovedErr *HeadMovedError
		if !errors.As(err, &headMovedErr) {
			t.Fatalf("expected *HeadMovedError, got %T: %v", err, err)
		}
		if headMovedErr.ActualHeadOID != actualOID {
			t.Errorf("got actual head %s, want %s", headMovedErr.ActualHeadOID, actualOID)
		}
	})

	t.Run("forbidden 403 error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Forbidden", http.StatusForbidden)
		}))
		defer srv.Close()

		client, err := NewTestClient(srv.URL)
		if err != nil {
			t.Fatal(err)
		}

		_, err = client.CommitFilesAtExpectedHead(context.Background(), owner, repo, branch, expectedOID, "test commit", map[string]string{"a.txt": "b"}, nil)
		if err == nil {
			t.Fatal("expected error on 403")
		}
		if !strings.Contains(err.Error(), "status 403") {
			t.Errorf("expected status 403 error, got %v", err)
		}
	})

	t.Run("oversized payload rejected client-side", func(t *testing.T) {
		client, err := NewTestClient("http://example.invalid")
		if err != nil {
			t.Fatal(err)
		}

		huge := strings.Repeat("A", 1024*1024+1)
		_, err = client.CommitFilesAtExpectedHead(context.Background(), owner, repo, branch, expectedOID, "test commit", map[string]string{"huge.bin": huge}, nil)
		if err == nil {
			t.Fatal("expected client-side rejection for oversized additions")
		}
		if !strings.Contains(err.Error(), "exceeds 1 MiB limit") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
