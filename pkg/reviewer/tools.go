package reviewer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// SnapshotTools provides confined read-only access to a repository snapshot.
type SnapshotTools struct {
	mu                sync.Mutex
	workDir           string
	maxReadBytes      int64
	maxToolBytes      int64
	maxSearchMatches  int
	bytesConsumed     int64
	budgetExhausted   bool
}

// NewSnapshotTools creates a new tool handler confined to workDir.
func NewSnapshotTools(workDir string, cfg *config.Config) (*SnapshotTools, error) {
	if workDir == "" {
		return nil, fmt.Errorf("workDir cannot be empty")
	}

	canonical, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("invalid workDir: %w", err)
	}
	if evaled, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = evaled
	}

	maxRead := int64(config.DefaultAgentFileReadBytes)
	maxTool := int64(config.DefaultAgentMaxToolBytes)
	maxMatches := config.DefaultAgentSearchMaxMatches

	if cfg != nil {
		if cfg.AgentFileReadBytes > 0 {
			maxRead = cfg.AgentFileReadBytes
		}
		if cfg.AgentMaxToolBytes > 0 {
			maxTool = cfg.AgentMaxToolBytes
		}
		if cfg.AgentSearchMaxMatches > 0 {
			maxMatches = cfg.AgentSearchMaxMatches
		}
	}

	return &SnapshotTools{
		workDir:          canonical,
		maxReadBytes:     maxRead,
		maxToolBytes:     maxTool,
		maxSearchMatches: maxMatches,
	}, nil
}

// BytesConsumed returns the total number of tool output bytes served.
func (st *SnapshotTools) BytesConsumed() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.bytesConsumed
}

// IsBudgetExhausted returns true if AGENT_MAX_TOOL_BYTES was reached.
func (st *SnapshotTools) IsBudgetExhausted() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.budgetExhausted
}

// debitAndBound bounds the raw tool output against the remaining byte budget.
func (st *SnapshotTools) debitAndBound(rawOutput string) string {
	st.mu.Lock()
	defer st.mu.Unlock()

	rawLen := int64(len(rawOutput))
	remaining := st.maxToolBytes - st.bytesConsumed
	if remaining <= 0 {
		st.budgetExhausted = true
		return "...[tool output omitted, budget exhausted]..."
	}

	if rawLen <= remaining {
		st.bytesConsumed += rawLen
		if st.bytesConsumed >= st.maxToolBytes {
			st.budgetExhausted = true
		}
		return rawOutput
	}

	// Truncate to fit remaining budget
	const marker = "\n...[tool output truncated, budget exhausted]..."
	markerLen := int64(len(marker))
	var truncated string
	if remaining > markerLen {
		truncated = rawOutput[:remaining-markerLen] + marker
	} else {
		truncated = marker[:remaining]
	}
	st.bytesConsumed += int64(len(truncated))
	st.budgetExhausted = true
	return truncated
}

// ReadFile reads a file confined to the snapshot root.
func (st *SnapshotTools) ReadFile(userPath string) (string, error) {
	if userPath == "" {
		return "", fmt.Errorf("empty path")
	}

	if strings.ContainsRune(userPath, 0) {
		return "", fmt.Errorf("invalid path: contains null byte")
	}
	if filepath.IsAbs(userPath) || filepath.VolumeName(userPath) != "" ||
		strings.HasPrefix(userPath, "/") || strings.HasPrefix(userPath, "\\") {
		return "", fmt.Errorf("absolute paths are not permitted")
	}

	cleanRel := filepath.Clean(userPath)
	slashRel := filepath.ToSlash(cleanRel)
	if slashRel == "." {
		return "", fmt.Errorf("cannot read directory as file")
	}
	if slashRel == ".." || strings.HasPrefix(slashRel, "../") {
		return "", fmt.Errorf("path outside workspace")
	}
	if isGitPath(slashRel) {
		return "", fmt.Errorf(".git paths are restricted")
	}

	root, err := os.OpenRoot(st.workDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	f, err := root.Open(cleanRel)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", fmt.Errorf("access denied (%w)", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("cannot read directory as file")
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("access denied: not a regular file")
	}

	if err := verifyFileHandle(f, st.workDir); err != nil {
		return "", fmt.Errorf("access denied: %w", err)
	}

	limited := io.LimitReader(f, st.maxReadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	var output string
	if int64(len(data)) > st.maxReadBytes {
		output = string(data[:st.maxReadBytes]) + fmt.Sprintf("\n...[file truncated, exceeded %d bytes]...", st.maxReadBytes)
	} else {
		output = string(data)
	}

	return st.debitAndBound(output), nil
}

// ListFiles lists up to 100 directory entries confined to the snapshot root.
func (st *SnapshotTools) ListFiles(userPath string) (string, error) {
	subPath := "."
	if userPath != "" && userPath != "." {
		if strings.ContainsRune(userPath, 0) {
			return "", fmt.Errorf("invalid path: contains null byte")
		}
		if filepath.IsAbs(userPath) || filepath.VolumeName(userPath) != "" ||
			strings.HasPrefix(userPath, "/") || strings.HasPrefix(userPath, "\\") {
			return "", fmt.Errorf("absolute paths are not permitted")
		}
		cleanRel := filepath.Clean(userPath)
		slashRel := filepath.ToSlash(cleanRel)
		if slashRel == ".." || strings.HasPrefix(slashRel, "../") {
			return "", fmt.Errorf("path outside workspace")
		}
		if isGitPath(slashRel) {
			return "", fmt.Errorf(".git paths are restricted")
		}
		subPath = slashRel
	}

	root, err := os.OpenRoot(st.workDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	if subPath != "." {
		fi, err := root.Stat(filepath.FromSlash(subPath))
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			return "", fmt.Errorf("access denied (%w)", err)
		}
		if !fi.IsDir() {
			return st.debitAndBound(filepath.FromSlash(subPath)), nil
		}
	}

	fsys := root.FS()
	var files []string
	err = fs.WalkDir(fsys, subPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == subPath {
				return walkErr
			}
			return nil
		}
		if len(files) >= 100 {
			return fs.SkipAll
		}

		slash := filepath.ToSlash(path)
		if isGitPath(slash) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if path != "." {
			files = append(files, filepath.FromSlash(path))
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	return st.debitAndBound(strings.Join(files, "\n")), nil
}

// SearchFiles searches text patterns confined to the snapshot root, skipping .git and binaries.
func (st *SnapshotTools) SearchFiles(query string) (string, error) {
	trimmedQuery := strings.TrimSpace(query)
	if trimmedQuery == "" {
		return "", fmt.Errorf("search pattern cannot be empty")
	}

	root, err := os.OpenRoot(st.workDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	fsys := root.FS()
	lowerQuery := strings.ToLower(trimmedQuery)
	var matches []string
	matchCount := 0

	// Bound per-file reading during search to avoid reading huge files fully into memory
	maxPerFile := st.maxReadBytes
	if maxPerFile <= 0 {
		maxPerFile = 30000
	}

	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if matchCount >= st.maxSearchMatches {
			return fs.SkipAll
		}

		slash := filepath.ToSlash(path)
		if isGitPath(slash) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		func() {
			// Open file through root to ensure containment
			f, err := root.Open(path)
			if err != nil {
				return
			}
			defer f.Close()

			// Verify handle
			if err := verifyFileHandle(f, st.workDir); err != nil {
				return
			}

			// Binary check: read first 512 bytes
			header := make([]byte, 512)
			n, err := f.Read(header)
			if err != nil && err != io.EOF {
				return
			}
			if n > 0 && bytes.IndexByte(header[:n], 0) != -1 {
				// Skip binary file
				return
			}

			// Rewind to beginning
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return
			}

			// Read bounded content
			limited := io.LimitReader(f, maxPerFile)
			content, err := io.ReadAll(limited)
			if err != nil {
				return
			}

			lines := strings.Split(string(content), "\n")
			for lineIdx, line := range lines {
				if strings.Contains(strings.ToLower(line), lowerQuery) {
					matchCount++
					cleanLine := strings.TrimRight(line, "\r")
					if len(cleanLine) > 200 {
						cleanLine = cleanLine[:200] + "..."
					}
					matches = append(matches, fmt.Sprintf("%s:%d: %s", filepath.FromSlash(path), lineIdx+1, cleanLine))
					if matchCount >= st.maxSearchMatches {
						return
					}
				}
			}
		}()

		if matchCount >= st.maxSearchMatches {
			return fs.SkipAll
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	if len(matches) == 0 {
		return st.debitAndBound(fmt.Sprintf("No matches found for %q", trimmedQuery)), nil
	}

	return st.debitAndBound(strings.Join(matches, "\n")), nil
}

func isGitPath(slashRel string) bool {
	lower := strings.ToLower(slashRel)
	parts := strings.Split(lower, "/")
	for _, part := range parts {
		cleanPart := strings.TrimRight(part, ". ")
		if cleanPart == ".git" || strings.HasPrefix(cleanPart, ".git:") {
			return true
		}
	}
	return lower == ".git" || strings.HasPrefix(lower, ".git/") ||
		strings.Contains(lower, "/.git/") || strings.HasSuffix(lower, "/.git")
}
