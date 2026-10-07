package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/github"
)

var (
	ErrCommitMismatch     = errors.New("full commit SHA mismatch between authenticated response and requested commit")
	ErrIncompleteTree     = errors.New("github tree listing is truncated and cannot be completely verified (SAFE-01)")
	ErrFileCountExceeded  = errors.New("snapshot exceeds maximum file count limit")
	ErrTotalBytesExceeded = errors.New("snapshot exceeds maximum total byte limit")
	ErrFileBytesExceeded  = errors.New("snapshot file exceeds maximum individual file byte limit")
	ErrPathDepthExceeded  = errors.New("snapshot file path exceeds maximum directory depth")
)

const (
	DefaultMaxAPIFiles      = 5000
	DefaultMaxAPITotalBytes = 100 * 1024 * 1024 // 100 MiB
	DefaultMaxAPIFileBytes  = 10 * 1024 * 1024  // 10 MiB
	DefaultMaxAPIPathDepth  = 32
)

// APISnapshotSource provides bounded, exact-commit GitHub API data snapshots
// on non-execution platforms (e.g. macOS and Windows) without host Git, hooks, or scripts.
type APISnapshotSource struct {
	Client        *github.Client
	Auth          RetrievalCredentialProvider
	HeadRepoID    int64
	IsPrivate     bool
	BaseDir       string
	MaxFiles      int
	MaxTotalBytes int64
	MaxFileBytes  int64
	MaxDepth      int
}

// NewAPISnapshotSource creates an APISnapshotSource with default safe bounds.
func NewAPISnapshotSource(client *github.Client, auth RetrievalCredentialProvider, headRepoID int64, isPrivate bool) *APISnapshotSource {
	return &APISnapshotSource{
		Client:        client,
		Auth:          auth,
		HeadRepoID:    headRepoID,
		IsPrivate:     isPrivate,
		MaxFiles:      DefaultMaxAPIFiles,
		MaxTotalBytes: DefaultMaxAPITotalBytes,
		MaxFileBytes:  DefaultMaxAPIFileBytes,
		MaxDepth:      DefaultMaxAPIPathDepth,
	}
}

func (s *APISnapshotSource) maxFiles() int {
	if s.MaxFiles > 0 {
		return s.MaxFiles
	}
	return DefaultMaxAPIFiles
}

func (s *APISnapshotSource) maxTotalBytes() int64 {
	if s.MaxTotalBytes > 0 {
		return s.MaxTotalBytes
	}
	return DefaultMaxAPITotalBytes
}

func (s *APISnapshotSource) maxFileBytes() int64 {
	if s.MaxFileBytes > 0 {
		return s.MaxFileBytes
	}
	return DefaultMaxAPIFileBytes
}

func (s *APISnapshotSource) maxDepth() int {
	if s.MaxDepth > 0 {
		return s.MaxDepth
	}
	return DefaultMaxAPIPathDepth
}

// PrepareSource implements the SourceProvider interface using bounded GitHub API calls.
func (s *APISnapshotSource) PrepareSource(ctx context.Context, cloneURL, headRef, headSHA string) (*Snapshot, func(), error) {
	// 1. Validate clone URL and full commit OID (SAFE-01)
	owner, repo, err := ValidateCloneURL(cloneURL)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateCommitOID(headSHA); err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(headRef, "-") {
		return nil, nil, fmt.Errorf("%w: option-like headRef rejected: %q", ErrInvalidSnapshot, headRef)
	}

	if s.Client == nil {
		return nil, nil, ErrSourceProviderUnavailable
	}

	// 2. Manage narrow App credential for private repositories (D-10)
	var scopedClient *github.Client = s.Client
	var revokeToken func()

	if s.IsPrivate {
		if s.Auth == nil {
			return nil, nil, ErrRetrievalAuthUnavailable
		}
		cred, err := s.Auth.CreateRetrievalCredential(ctx, owner, repo, s.HeadRepoID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create retrieval credential: %w", err)
		}

		var credRevoked bool
		revokeToken = func() {
			if !credRevoked && cred != nil {
				credRevoked = true
				cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Auth.RevokeRetrievalCredential(cleanCtx, cred)
			}
		}

		scopedClient = s.Client.NewScopedClient(cred.Token)
	}

	fail := func(err error) (*Snapshot, func(), error) {
		if revokeToken != nil {
			revokeToken()
		}
		return nil, nil, err
	}

	// 3. Verify exact commit equality via authenticated GitHub response
	commit, err := scopedClient.GetCommit(ctx, owner, repo, headSHA)
	if err != nil {
		return fail(fmt.Errorf("failed to get commit %s: %w", headSHA, err))
	}
	actualSHA := commit.GetSHA()
	if !strings.EqualFold(actualSHA, headSHA) {
		return fail(fmt.Errorf("%w: expected %s, got %s", ErrCommitMismatch, headSHA, actualSHA))
	}

	// 4. Retrieve complete tree, handling truncation if necessary
	tree, err := scopedClient.GetTree(ctx, owner, repo, headSHA, true)
	if err != nil {
		return fail(fmt.Errorf("failed to get tree for %s: %w", headSHA, err))
	}

	entries := tree.Entries
	if tree.GetTruncated() {
		pagedEntries, err := s.traverseTreeNonRecursive(ctx, scopedClient, owner, repo, headSHA)
		if err != nil {
			return fail(err)
		}
		entries = pagedEntries
	}

	if len(entries) > s.maxFiles() {
		return fail(fmt.Errorf("%w: got %d entries, limit is %d", ErrFileCountExceeded, len(entries), s.maxFiles()))
	}

	// 5. Materialize snapshot into confined temporary directory
	baseDir := s.BaseDir
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	destDir, err := os.MkdirTemp(baseDir, fmt.Sprintf("pr-snap-%s-*", headSHA[:10]))
	if err != nil {
		return fail(fmt.Errorf("failed to create snapshot directory: %w", err))
	}

	cleanup := func() {
		_ = os.RemoveAll(destDir)
		if revokeToken != nil {
			revokeToken()
		}
	}

	failClean := func(err error) (*Snapshot, func(), error) {
		cleanup()
		return nil, nil, err
	}

	manifest := make(map[string]string)
	var totalBytes int64
	maxFile := s.maxFileBytes()
	maxTotal := s.maxTotalBytes()
	maxDepth := s.maxDepth()

	for _, entry := range entries {
		entryPath := entry.GetPath()
		if entryPath == "" {
			continue
		}

		if err := ValidateSnapshotPath(entryPath); err != nil {
			return failClean(err)
		}

		segments := strings.Split(entryPath, "/")
		if len(segments) > maxDepth {
			return failClean(fmt.Errorf("%w: path %q depth %d exceeds max %d", ErrPathDepthExceeded, entryPath, len(segments), maxDepth))
		}

		mode := entry.GetMode()
		entryType := entry.GetType()

		// Submodule / gitlink check (mode 160000 or type commit)
		if mode == "160000" || entryType == "commit" {
			cleanup()
			return &Snapshot{
				CommitSHA:        headSHA,
				SourceDir:        destDir,
				IsIncomplete:     true,
				IncompleteReason: fmt.Sprintf("gitlink/submodule detected at %s", entryPath),
			}, func() {}, fmt.Errorf("%w: %s", ErrIncompleteGitlink, entryPath)
		}

		// Symlink check (mode 120000)
		if mode == "120000" {
			blob, err := scopedClient.GetBlob(ctx, owner, repo, entry.GetSHA())
			if err != nil {
				return failClean(fmt.Errorf("failed to read symlink blob for %s: %w", entryPath, err))
			}
			blobBytes, err := decodeBlobContent(blob)
			if err != nil {
				return failClean(err)
			}
			target := string(blobBytes)
			if err := ValidateSymlinkTarget(entryPath, target); err != nil {
				return failClean(err)
			}

			targetFsPath := filepath.Join(destDir, filepath.FromSlash(entryPath))
			if err := os.MkdirAll(filepath.Dir(targetFsPath), 0755); err != nil {
				return failClean(err)
			}
			_ = os.Remove(targetFsPath)
			if err := os.Symlink(target, targetFsPath); err != nil {
				// On systems where symlinks fail (e.g. non-elevated Windows), skip creation if safe
				continue
			}
			continue
		}

		// Regular or executable file (blob)
		if entryType == "blob" || mode == "100644" || mode == "100755" {
			blob, err := scopedClient.GetBlob(ctx, owner, repo, entry.GetSHA())
			if err != nil {
				return failClean(fmt.Errorf("failed to read blob for %s: %w", entryPath, err))
			}
			blobBytes, err := decodeBlobContent(blob)
			if err != nil {
				return failClean(err)
			}

			blobLen := int64(len(blobBytes))
			if blobLen > maxFile {
				return failClean(fmt.Errorf("%w: file %s size %d exceeds limit %d", ErrFileBytesExceeded, entryPath, blobLen, maxFile))
			}
			if totalBytes+blobLen > maxTotal {
				return failClean(fmt.Errorf("%w: total size %d exceeds limit %d", ErrTotalBytesExceeded, totalBytes+blobLen, maxTotal))
			}
			totalBytes += blobLen

			// Check for Git LFS pointer
			if bytes.HasPrefix(blobBytes, []byte("version https://git-lfs.github.com/spec/v1\n")) {
				cleanup()
				return &Snapshot{
					CommitSHA:        headSHA,
					SourceDir:        destDir,
					IsIncomplete:     true,
					IncompleteReason: fmt.Sprintf("git LFS pointer detected at %s", entryPath),
				}, func() {}, fmt.Errorf("%w: %s", ErrIncompleteGitLFS, entryPath)
			}

			targetFsPath := filepath.Join(destDir, filepath.FromSlash(entryPath))
			if err := os.MkdirAll(filepath.Dir(targetFsPath), 0755); err != nil {
				return failClean(err)
			}

			fileMode := os.FileMode(0644)
			if mode == "100755" {
				fileMode = 0755
			}
			if err := os.WriteFile(targetFsPath, blobBytes, fileMode); err != nil {
				return failClean(fmt.Errorf("failed to write snapshot file %s: %w", targetFsPath, err))
			}

			h := sha256.Sum256(blobBytes)
			manifest[entryPath] = hex.EncodeToString(h[:])
			continue
		}
	}

	snapshot := &Snapshot{
		CommitSHA: headSHA,
		SourceDir: destDir,
		Manifest:  manifest,
	}

	if err := snapshot.Validate(); err != nil {
		return failClean(fmt.Errorf("prepared snapshot validation failed: %w", err))
	}

	// Revoke credential promptly upon successful snapshot creation (D-10)
	if revokeToken != nil {
		revokeToken()
		revokeToken = nil
	}

	finalCleanup := func() {
		_ = os.RemoveAll(destDir)
	}

	return snapshot, finalCleanup, nil
}

type treeQueueItem struct {
	sha    string
	prefix string
	depth  int
}

func (s *APISnapshotSource) traverseTreeNonRecursive(ctx context.Context, client *github.Client, owner, repo, rootSHA string) ([]*gh.TreeEntry, error) {
	var allEntries []*gh.TreeEntry
	queue := []treeQueueItem{{sha: rootSHA, prefix: "", depth: 1}}
	maxDepth := s.maxDepth()
	maxFiles := s.maxFiles()

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if item.depth > maxDepth {
			return nil, fmt.Errorf("%w: directory traversal exceeded max depth %d at %s", ErrPathDepthExceeded, maxDepth, item.prefix)
		}

		t, err := client.GetTree(ctx, owner, repo, item.sha, false)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch subtree for %s (%s): %w", item.prefix, item.sha, err)
		}
		if t.GetTruncated() {
			return nil, fmt.Errorf("%w: subtree at %q was truncated by GitHub API", ErrIncompleteTree, item.prefix)
		}

		for _, e := range t.Entries {
			entryPath := e.GetPath()
			if item.prefix != "" {
				entryPath = item.prefix + "/" + entryPath
			}
			e.Path = gh.Ptr(entryPath)

			if e.GetType() == "tree" {
				queue = append(queue, treeQueueItem{
					sha:    e.GetSHA(),
					prefix: entryPath,
					depth:  item.depth + 1,
				})
			} else {
				allEntries = append(allEntries, e)
				if len(allEntries) > maxFiles {
					return nil, fmt.Errorf("%w: exceeded %d entries during non-recursive traversal", ErrFileCountExceeded, maxFiles)
				}
			}
		}
	}

	return allEntries, nil
}

func decodeBlobContent(blob *gh.Blob) ([]byte, error) {
	if blob == nil {
		return nil, errors.New("blob is nil")
	}
	content := blob.GetContent()
	encoding := blob.GetEncoding()

	if strings.EqualFold(encoding, "base64") {
		// GitHub base64 blob content may contain newlines
		cleaned := strings.ReplaceAll(content, "\n", "")
		cleaned = strings.ReplaceAll(cleaned, "\r", "")
		data, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("failed to decode base64 blob: %w", err)
		}
		return data, nil
	}

	return []byte(content), nil
}
