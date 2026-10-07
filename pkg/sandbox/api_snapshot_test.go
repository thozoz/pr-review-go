package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/github"
)

type mockAPIServer struct {
	server       *httptest.Server
	commitSHA    string
	treeEntries  []*gh.TreeEntry
	blobs        map[string][]byte
	isTruncated  bool
	subtrees     map[string][]*gh.TreeEntry
	authHeader   string
	receivedAuth []string
	mu           sync.Mutex
}

func newMockAPIServer(commitSHA string) *mockAPIServer {
	m := &mockAPIServer{
		commitSHA: commitSHA,
		blobs:     make(map[string][]byte),
		subtrees:  make(map[string][]*gh.TreeEntry),
	}

	mux := http.NewServeMux()

	// GetCommit endpoint: /repos/{owner}/{repo}/commits/{sha}
	mux.HandleFunc("/repos/testowner/testrepo/commits/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.receivedAuth = append(m.receivedAuth, r.Header.Get("Authorization"))
		sha := strings.TrimPrefix(r.URL.Path, "/repos/testowner/testrepo/commits/")
		expected := m.commitSHA
		m.mu.Unlock()

		if strings.EqualFold(sha, expected) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"sha": expected,
			})
			return
		}

		// If mismatching commit requested or returned
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sha": "different-sha-returned",
		})
	})

	// GetTree endpoint: /repos/{owner}/{repo}/git/trees/{sha}
	mux.HandleFunc("/repos/testowner/testrepo/git/trees/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.receivedAuth = append(m.receivedAuth, r.Header.Get("Authorization"))
		sha := strings.TrimPrefix(r.URL.Path, "/repos/testowner/testrepo/git/trees/")
		isRec := r.URL.Query().Get("recursive") == "1"
		var entries []*gh.TreeEntry
		var trunc bool
		if isRec {
			entries = m.treeEntries
			trunc = m.isTruncated
		} else {
			if sub, ok := m.subtrees[sha]; ok {
				entries = sub
				trunc = false
			} else {
				entries = m.treeEntries
				trunc = m.isTruncated
			}
		}
		m.mu.Unlock()

		_ = isRec
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sha":       sha,
			"truncated": trunc,
			"tree":      entries,
		})
	})

	// GetBlob endpoint: /repos/{owner}/{repo}/git/blobs/{sha}
	mux.HandleFunc("/repos/testowner/testrepo/git/blobs/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.receivedAuth = append(m.receivedAuth, r.Header.Get("Authorization"))
		sha := strings.TrimPrefix(r.URL.Path, "/repos/testowner/testrepo/git/blobs/")
		content, ok := m.blobs[sha]
		m.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}

		encoded := base64.StdEncoding.EncodeToString(content)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"sha":      sha,
			"size":     len(content),
			"encoding": "base64",
			"content":  encoded,
		})
	})

	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockAPIServer) Close() {
	m.server.Close()
}

type apiMockCredentialProvider struct {
	createdTokens []*github.RetrievalCredential
	revokedTokens []*github.RetrievalCredential
	createErr     error
	mu            sync.Mutex
}

func (p *apiMockCredentialProvider) CreateRetrievalCredential(ctx context.Context, owner, repo string, repoID int64) (*github.RetrievalCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.createErr != nil {
		return nil, p.createErr
	}
	cred := &github.RetrievalCredential{
		Token:        "mock-retrieval-app-token-12345",
		RepositoryID: repoID,
		RepoName:     repo,
		ExpiresAt:    time.Now().Add(10 * time.Minute),
	}
	p.createdTokens = append(p.createdTokens, cred)
	return cred, nil
}

func (p *apiMockCredentialProvider) RevokeRetrievalCredential(ctx context.Context, cred *github.RetrievalCredential) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revokedTokens = append(p.revokedTokens, cred)
	cred.Zeroize()
	return nil
}

func TestAPISnapshotSource_CompleteCommitTreeBlobRetrieval(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	blob1SHA := "1111111111111111111111111111111111111111"
	blob2SHA := "2222222222222222222222222222222222222222"
	mock.blobs[blob1SHA] = []byte("package main\n\nfunc main() {}\n")
	mock.blobs[blob2SHA] = []byte("module example.com/test\n\ngo 1.22\n")

	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("main.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blob1SHA),
			Size: gh.Ptr(len(mock.blobs[blob1SHA])),
		},
		{
			Path: gh.Ptr("go.mod"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blob2SHA),
			Size: gh.Ptr(len(mock.blobs[blob2SHA])),
		},
	}

	ghClient, err := github.NewTestClient(mock.server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	source := NewAPISnapshotSource(ghClient, nil, 0, false)
	ctx := context.Background()

	cloneURL := "https://github.com/testowner/testrepo.git"
	snapshot, cleanup, err := source.PrepareSource(ctx, cloneURL, "main", commitSHA)
	if err != nil {
		t.Fatalf("PrepareSource failed: %v", err)
	}
	defer cleanup()

	if snapshot.CommitSHA != commitSHA {
		t.Errorf("expected commit %s, got %s", commitSHA, snapshot.CommitSHA)
	}

	// Verify files materialized
	mainPath := filepath.Join(snapshot.SourceDir, "main.go")
	data, err := os.ReadFile(mainPath)
	if err != nil || string(data) != string(mock.blobs[blob1SHA]) {
		t.Errorf("main.go mismatch: %v, content: %s", err, string(data))
	}

	// Verify manifest
	if len(snapshot.Manifest) != 2 {
		t.Errorf("expected manifest with 2 entries, got %d", len(snapshot.Manifest))
	}
	if snapshot.Manifest["main.go"] == "" || snapshot.Manifest["go.mod"] == "" {
		t.Errorf("missing entries in manifest: %+v", snapshot.Manifest)
	}

	// Verify cleanup removes directory
	srcDir := snapshot.SourceDir
	cleanup()
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove snapshot directory %s", srcDir)
	}
}

func TestAPISnapshotSource_CommitOIDMismatch_Denied(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	ghClient, err := github.NewTestClient(mock.server.URL)
	if err != nil {
		t.Fatal(err)
	}

	source := NewAPISnapshotSource(ghClient, nil, 0, false)
	ctx := context.Background()

	// Request a different commit SHA than what mock API returns
	requestedSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, _, err = source.PrepareSource(ctx, "https://github.com/testowner/testrepo.git", "main", requestedSHA)
	if err == nil {
		t.Fatalf("expected commit mismatch error, got nil")
	}
	if !errors.Is(err, ErrCommitMismatch) && !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected commit mismatch error, got: %v", err)
	}
}

func TestAPISnapshotSource_InvalidCommitOID_Denied(t *testing.T) {
	source := NewAPISnapshotSource(nil, nil, 0, false)
	ctx := context.Background()

	// Short prefix SHA
	_, _, err := source.PrepareSource(ctx, "https://github.com/testowner/testrepo.git", "main", "abcdef0")
	if err == nil || !errors.Is(err, ErrInvalidCommitOID) {
		t.Errorf("expected ErrInvalidCommitOID for short prefix, got: %v", err)
	}

	// Non-hex characters
	_, _, err = source.PrepareSource(ctx, "https://github.com/testowner/testrepo.git", "main", "0123456789abcdef0123456789abcdef0123456z")
	if err == nil || !errors.Is(err, ErrInvalidCommitOID) {
		t.Errorf("expected ErrInvalidCommitOID for non-hex, got: %v", err)
	}
}

func TestAPISnapshotSource_TruncatedTree_HandlesNonRecursive(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	mock.isTruncated = true // root tree reports truncated
	blobSHA := "3333333333333333333333333333333333333333"
	mock.blobs[blobSHA] = []byte("package sub\n")

	subTreeSHA := "4444444444444444444444444444444444444444"
	mock.subtrees[commitSHA] = []*gh.TreeEntry{
		{
			Path: gh.Ptr("pkg"),
			Mode: gh.Ptr("040000"),
			Type: gh.Ptr("tree"),
			SHA:  gh.Ptr(subTreeSHA),
		},
	}
	mock.subtrees[subTreeSHA] = []*gh.TreeEntry{
		{
			Path: gh.Ptr("sub.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blobSHA),
			Size: gh.Ptr(len(mock.blobs[blobSHA])),
		},
	}

	ghClient, err := github.NewTestClient(mock.server.URL)
	if err != nil {
		t.Fatal(err)
	}

	source := NewAPISnapshotSource(ghClient, nil, 0, false)
	ctx := context.Background()

	snap, cleanup, err := source.PrepareSource(ctx, "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if err != nil {
		t.Fatalf("PrepareSource failed on non-recursive resolution: %v", err)
	}
	defer cleanup()

	target := filepath.Join(snap.SourceDir, "pkg", "sub.go")
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected pkg/sub.go to be materialized: %v", err)
	}
}

func TestAPISnapshotSource_BoundsEnforcement(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	blobSHA := "5555555555555555555555555555555555555555"
	mock.blobs[blobSHA] = []byte("hello world")

	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("file1.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blobSHA),
			Size: gh.Ptr(11),
		},
		{
			Path: gh.Ptr("file2.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blobSHA),
			Size: gh.Ptr(11),
		},
	}

	ghClient, _ := github.NewTestClient(mock.server.URL)

	// 1. MaxFiles exceeded
	source := NewAPISnapshotSource(ghClient, nil, 0, false)
	source.MaxFiles = 1
	_, _, err := source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if !errors.Is(err, ErrFileCountExceeded) {
		t.Errorf("expected ErrFileCountExceeded, got: %v", err)
	}

	// 2. MaxTotalBytes exceeded
	source = NewAPISnapshotSource(ghClient, nil, 0, false)
	source.MaxTotalBytes = 15
	_, _, err = source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if !errors.Is(err, ErrTotalBytesExceeded) {
		t.Errorf("expected ErrTotalBytesExceeded, got: %v", err)
	}

	// 3. MaxFileBytes exceeded
	source = NewAPISnapshotSource(ghClient, nil, 0, false)
	source.MaxFileBytes = 5
	_, _, err = source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if !errors.Is(err, ErrFileBytesExceeded) {
		t.Errorf("expected ErrFileBytesExceeded, got: %v", err)
	}

	// 4. MaxDepth exceeded
	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("a/b/c/d.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blobSHA),
			Size: gh.Ptr(11),
		},
	}
	source = NewAPISnapshotSource(ghClient, nil, 0, false)
	source.MaxDepth = 3
	_, _, err = source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if !errors.Is(err, ErrPathDepthExceeded) {
		t.Errorf("expected ErrPathDepthExceeded, got: %v", err)
	}
}

func TestAPISnapshotSource_UnsafePath_WindowsReservedAndTraversal(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	blobSHA := "6666666666666666666666666666666666666666"
	mock.blobs[blobSHA] = []byte("data")

	ghClient, _ := github.NewTestClient(mock.server.URL)

	testCases := []struct {
		name string
		path string
	}{
		{"CON reserved", "con.txt"},
		{"AUX reserved", "aux"},
		{"COM1 reserved", "nested/com1.go"},
		{"trailing dot", "safe./file.go"},
		{"trailing space", "bad /file.go"},
		{".git reference", ".git/config"},
		{"traversal segment", "a/../b.go"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mock.treeEntries = []*gh.TreeEntry{
				{
					Path: gh.Ptr(tc.path),
					Mode: gh.Ptr("100644"),
					Type: gh.Ptr("blob"),
					SHA:  gh.Ptr(blobSHA),
					Size: gh.Ptr(4),
				},
			}

			source := NewAPISnapshotSource(ghClient, nil, 0, false)
			_, _, err := source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
			if err == nil || !errors.Is(err, ErrUnsafePathEntry) {
				t.Errorf("expected ErrUnsafePathEntry for %q, got: %v", tc.path, err)
			}
		})
	}
}

func TestAPISnapshotSource_GitlinkSubmodule_DeniedIncomplete(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("submodule_repo"),
			Mode: gh.Ptr("160000"),
			Type: gh.Ptr("commit"),
			SHA:  gh.Ptr("7777777777777777777777777777777777777777"),
		},
	}

	ghClient, _ := github.NewTestClient(mock.server.URL)
	source := NewAPISnapshotSource(ghClient, nil, 0, false)

	snap, _, err := source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if err == nil || !errors.Is(err, ErrIncompleteGitlink) {
		t.Errorf("expected ErrIncompleteGitlink, got: %v", err)
	}
	if snap == nil || !snap.IsIncomplete {
		t.Errorf("expected snapshot.IsIncomplete to be true")
	}
}

func TestAPISnapshotSource_GitLFSPointer_DeniedIncomplete(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	lfsBlobSHA := "8888888888888888888888888888888888888888"
	lfsPointerContent := "version https://git-lfs.github.com/spec/v1\noid sha256:4d7a214614ab2935c943f9e0ff69d22eca54f3441c0ee0905507b61f783f071f\nsize 12345\n"
	mock.blobs[lfsBlobSHA] = []byte(lfsPointerContent)

	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("large_asset.bin"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(lfsBlobSHA),
			Size: gh.Ptr(len(lfsPointerContent)),
		},
	}

	ghClient, _ := github.NewTestClient(mock.server.URL)
	source := NewAPISnapshotSource(ghClient, nil, 0, false)

	snap, _, err := source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if err == nil || !errors.Is(err, ErrIncompleteGitLFS) {
		t.Errorf("expected ErrIncompleteGitLFS, got: %v", err)
	}
	if snap == nil || !snap.IsIncomplete {
		t.Errorf("expected snapshot.IsIncomplete to be true")
	}
}

func TestAPISnapshotSource_PrivateRepo_TokenLifecycle(t *testing.T) {
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	mock := newMockAPIServer(commitSHA)
	defer mock.Close()

	blobSHA := "9999999999999999999999999999999999999999"
	mock.blobs[blobSHA] = []byte("package priv\n")
	mock.treeEntries = []*gh.TreeEntry{
		{
			Path: gh.Ptr("priv.go"),
			Mode: gh.Ptr("100644"),
			Type: gh.Ptr("blob"),
			SHA:  gh.Ptr(blobSHA),
			Size: gh.Ptr(len(mock.blobs[blobSHA])),
		},
	}

	ghClient, _ := github.NewTestClient(mock.server.URL)
	credProvider := &apiMockCredentialProvider{}

	source := NewAPISnapshotSource(ghClient, credProvider, 42, true)
	ctx := context.Background()

	snap, cleanup, err := source.PrepareSource(ctx, "https://github.com/testowner/testrepo.git", "main", commitSHA)
	if err != nil {
		t.Fatalf("PrepareSource failed for private repo: %v", err)
	}
	defer cleanup()

	// Verify token was used
	mock.mu.Lock()
	usedTokens := mock.receivedAuth
	mock.mu.Unlock()

	foundAuth := false
	for _, a := range usedTokens {
		if strings.Contains(a, "mock-retrieval-app-token-12345") {
			foundAuth = true
			break
		}
	}
	if !foundAuth {
		t.Errorf("scoped token was not used in API calls: %v", usedTokens)
	}

	// Verify token was promptly revoked upon completion
	credProvider.mu.Lock()
	numCreated := len(credProvider.createdTokens)
	numRevoked := len(credProvider.revokedTokens)
	credProvider.mu.Unlock()

	if numCreated != 1 || numRevoked != 1 {
		t.Errorf("expected exactly 1 token created and 1 revoked, got %d created / %d revoked", numCreated, numRevoked)
	}

	_ = snap
}

func TestAPISnapshotSource_PrivateRepo_DeniedWithoutAuth(t *testing.T) {
	ghClient, _ := github.NewTestClient("https://github.com")
	source := NewAPISnapshotSource(ghClient, nil, 42, true)

	_, _, err := source.PrepareSource(context.Background(), "https://github.com/testowner/testrepo.git", "main", "0123456789abcdef0123456789abcdef01234567")
	if !errors.Is(err, ErrRetrievalAuthUnavailable) {
		t.Errorf("expected ErrRetrievalAuthUnavailable for private repo without auth, got: %v", err)
	}
}
