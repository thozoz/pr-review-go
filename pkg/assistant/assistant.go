package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

type Assistant struct {
	cfg     *config.Config
	gh      *github.Client
	sandbox *sandbox.Runner
	llm     *llm.Client
}

func NewAssistant(cfg *config.Config) *Assistant {
	gh := github.NewClientFromConfig(cfg)
	runner := sandbox.NewPlatformRunner(cfg, gh, nil, nil)
	return &Assistant{
		cfg:     cfg,
		gh:      gh,
		sandbox: runner,
		llm:     llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// SetRunner injects a configured runner for testing or custom environments.
func (a *Assistant) SetRunner(r *sandbox.Runner) {
	a.sandbox = r
}

type ToolCallRequest struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	FinalText string `json:"final_text"`
}

// HandleMention executes an interactive multi-step question/command regarding a PR
func (a *Assistant) HandleMention(ctx context.Context, owner, repo string, number int, userQuestion string) (string, error) {
	pr, err := a.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("failed to get PR: %w", err)
	}

	// Prepare sealed sandbox snapshot
	snapshot, cleanup, err := a.sandbox.PrepareSnapshot(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("failed to prepare sandbox snapshot: %w", err)
	}
	defer cleanup()
	workDir := snapshot.SourceDir

	systemPrompt := `You are an AI Coding Assistant operating on a Git Pull Request.
You can inspect code, review changes, suggest fixes, and answer questions about the PR.

AVAILABLE ACTIONS:
1. "read_file": Read content of a file. ({"action": "read_file", "path": "path/to/file"})
2. "list_files": List files in a directory. ({"action": "list_files", "path": "."})
3. "answer": Deliver your final response to the user. ({"action": "answer", "final_text": "Markdown summary of what was done or answered..."})

GENERAL AGENT RULES:
- Inspect relevant files to answer questions, diagnose issues, or suggest fixes.
- Direct command execution, file modifications, and automated git push are disabled because the interactive assistant is strictly read-only.
- You can make up to 6 iterative tool steps before providing your final answer.
- Output MUST be a single strict JSON object matching: {"action": "...", ...}
- Never include markdown codeblocks surrounding your JSON tool calls.`

	history := fmt.Sprintf("User Instruction: %s\nPR Title: %s (#%d)\nPR Branch: %s\nBase Branch: %s\n\n",
		userQuestion, pr.Title, number, pr.HeadRef, pr.BaseRef)

	// Tool execution loop (max 6 turns)
	for i := 0; i < 6; i++ {
		rawResponse, err := a.llm.ChatCompletion(ctx, systemPrompt, history)
		if err != nil {
			return "", fmt.Errorf("llm assistant step failed: %w", err)
		}

		step, err := parseToolCall(rawResponse)
		if err != nil {
			// Fallback: if model returned plain text instead of JSON, treat as final answer
			return rawResponse, nil
		}

		if step.Action == "answer" {
			return step.FinalText, nil
		}

		// Execute tool
		toolOutput := a.executeTool(ctx, workDir, step)
		history += fmt.Sprintf("\nAction requested: %s (path: %s)\nTool Result:\n```\n%s\n```\nNext action (or provide final answer):",
			step.Action, step.Path, toolOutput)
	}

	// Final prompt to conclude
	finalResp, err := a.llm.ChatCompletion(ctx, systemPrompt, history+"\nProvide your final comprehensive answer now via action='answer'.")
	if err != nil {
		return "", err
	}
	step, err := parseToolCall(finalResp)
	if err == nil && step.Action == "answer" {
		return step.FinalText, nil
	}
	return finalResp, nil
}

func (a *Assistant) executeTool(ctx context.Context, workDir string, step *ToolCallRequest) string {
	switch step.Action {
	case "read_file":
		content, err := readWorkspaceFile(workDir, step.Path)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
				return fmt.Sprintf("Error reading file: %v", err)
			}
			return fmt.Sprintf("Error: Access denied (%v).", err)
		}
		return content

	case "write_file":
		return "Error: write_file is disabled: the assistant is strictly read-only and file modifications are not permitted."

	case "list_files":
		p := step.Path
		if p == "" {
			p = "."
		}
		listing, err := listWorkspaceFiles(workDir, p)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
				return fmt.Sprintf("Error listing files: %v", err)
			}
			return fmt.Sprintf("Error: Access denied (%v).", err)
		}
		return listing

	case "run_command":
		return "Error: run_command is disabled: host command execution is not permitted without container isolation; assistant remains read-only in Phase 1."

	case "commit_and_push":
		return "Error: commit_and_push is disabled: git operations are not permitted without container isolation; assistant remains read-only in Phase 1."

	default:
		return fmt.Sprintf("Unknown action: %s", step.Action)
	}
}

// readWorkspaceFile opens and reads a file strictly within workDir.
// It uses os.OpenRoot to confine file operations to the workspace root,
// preventing path traversal and symlink escapes outside the workspace.
// Once opened, verifyFileHandle inspects the underlying open handle for
// defense-in-depth, and contents are read directly from the descriptor.
//
// Limitations and atomicity note:
// This approach substantially mitigates the check-then-open symlink swap window by reading
// directly from the opened descriptor rather than re-resolving by string path. However, it does
// NOT provide an absolute TOCTOU guarantee: on platforms without kernel-enforced atomic root
// containment (such as Linux openat2 with RESOLVE_BENEATH), os.Root relies on component-by-component
// traversal (using openat/O_NOFOLLOW on Unix and windows.Openat/O_NOFOLLOW_ANY on Windows).
// Consequently, residual race windows remain if a concurrent hostile local process performs
// high-frequency ancestor directory renames during path traversal.
func readWorkspaceFile(workDir, userPath string) (string, error) {
	if userPath == "" {
		return "", fmt.Errorf("empty path")
	}

	canonicalWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("invalid workspace: %w", err)
	}
	if evaled, err := filepath.EvalSymlinks(canonicalWorkDir); err == nil {
		canonicalWorkDir = evaled
	}

	// Lexical pre-checks
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

	// Open workspace root for confined filesystem access
	root, err := os.OpenRoot(canonicalWorkDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	// Open file strictly relative to root
	f, err := root.Open(cleanRel)
	if err != nil {
		if os.IsNotExist(err) {
			return "", err
		}
		// Root escape, symlink loop, or permission error is an access denial
		return "", fmt.Errorf("access denied (%w)", err)
	}
	defer f.Close()

	// Verify opened file attributes
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

	// Post-open handle verification: query the OS kernel for the true target
	// of the already-opened handle/descriptor to guarantee it is within canonicalWorkDir
	// and does not point to restricted locations (e.g. .git).
	if err := verifyFileHandle(f, canonicalWorkDir); err != nil {
		return "", fmt.Errorf("access denied: %w", err)
	}

	// Read directly from the opened handle (limit to 30KB + 1 to detect truncation).
	// This avoids any second path resolution (check-then-open TOCTOU).
	const maxBytes = 30000
	limited := io.LimitReader(f, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	if len(data) > maxBytes {
		return string(data[:maxBytes]) + "\n...[file truncated, exceeded 30KB]...", nil
	}
	return string(data), nil
}

// listWorkspaceFiles lists files strictly within workDir under userPath.
func listWorkspaceFiles(workDir, userPath string) (string, error) {
	canonicalWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("invalid workspace: %w", err)
	}
	if evaled, err := filepath.EvalSymlinks(canonicalWorkDir); err == nil {
		canonicalWorkDir = evaled
	}

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

	root, err := os.OpenRoot(canonicalWorkDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	if subPath != "." {
		fi, err := root.Stat(filepath.FromSlash(subPath))
		if err != nil {
			if os.IsNotExist(err) {
				return "", err
			}
			return "", fmt.Errorf("access denied (%w)", err)
		}
		if !fi.IsDir() {
			return filepath.FromSlash(subPath), nil
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
		if len(files) > 100 {
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
			rel := filepath.FromSlash(path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return strings.Join(files, "\n"), nil
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

func parseToolCall(raw string) (*ToolCallRequest, error) {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	}

	var req ToolCallRequest
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// RunAndReply handles @bot question and posts response as a comment
func (a *Assistant) RunAndReply(ctx context.Context, owner, repo string, number int, userQuestion string) (string, error) {
	answer, err := a.HandleMention(ctx, owner, repo, number, userQuestion)
	if err != nil {
		return "", err
	}

	body := fmt.Sprintf("### 🤖 Assistant Reply:\n\n%s", answer)
	if err := a.gh.PostComment(ctx, owner, repo, number, body); err != nil {
		return "", fmt.Errorf("failed to post assistant reply: %w", err)
	}
	return answer, nil
}
