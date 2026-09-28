package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	return &Assistant{
		cfg:     cfg,
		gh:      github.NewClientFromConfig(cfg),
		sandbox: sandbox.NewRunner(2 * time.Minute),
		llm:     llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
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

	// Prepare sandbox workspace
	workDir, cleanup, err := a.sandbox.PrepareWorkspace(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("failed to prepare sandbox workspace: %w", err)
	}
	defer cleanup()

	systemPrompt := `You are an AI Coding Assistant operating on a Git Pull Request.
You can inspect code, review changes, suggest fixes, and answer questions about the PR.

AVAILABLE ACTIONS:
1. "read_file": Read content of a file. ({"action": "read_file", "path": "path/to/file"})
2. "list_files": List files in a directory. ({"action": "list_files", "path": "."})
3. "answer": Deliver your final response to the user. ({"action": "answer", "final_text": "Markdown summary of what was done or answered..."})

GENERAL AGENT RULES:
- Inspect relevant files to answer questions, diagnose issues, or suggest fixes.
- Direct command execution, file modifications, and automated git push are disabled because the assistant is read-only and the current environment lacks container isolation.
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
		target, err := resolveWorkspacePath(workDir, step.Path)
		if err != nil {
			return fmt.Sprintf("Error: Access denied (%v).", err)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return fmt.Sprintf("Error reading file: %v", err)
		}
		if len(data) > 30000 {
			return string(data[:30000]) + "\n...[file truncated, exceeded 30KB]..."
		}
		return string(data)

	case "write_file":
		return "Error: write_file is disabled: the assistant is read-only and file modifications are not permitted."

	case "list_files":
		p := step.Path
		if p == "" {
			p = "."
		}
		target, err := resolveWorkspacePath(workDir, p)
		if err != nil {
			return fmt.Sprintf("Error: Access denied (%v).", err)
		}
		var files []string
		_ = filepath.Walk(target, func(walkedPath string, info os.FileInfo, err error) error {
			if err != nil || len(files) > 100 {
				return nil
			}
			rel, _ := filepath.Rel(workDir, walkedPath)
			if rel != "." && !isGitPath(filepath.ToSlash(rel)) {
				files = append(files, rel)
			}
			return nil
		})
		return strings.Join(files, "\n")

	case "run_command":
		return "Error: run_command is disabled: host command execution is not permitted without container isolation."

	case "commit_and_push":
		return "Error: commit_and_push is disabled: git operations are not permitted without container isolation."

	default:
		return fmt.Sprintf("Unknown action: %s", step.Action)
	}
}

func resolveWorkspacePath(workDir, userPath string) (string, error) {
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

	cleanUserPath := filepath.Clean(userPath)
	if filepath.IsAbs(cleanUserPath) || filepath.VolumeName(cleanUserPath) != "" {
		return "", fmt.Errorf("absolute paths are not permitted")
	}

	target := filepath.Join(canonicalWorkDir, cleanUserPath)

	// Lexical boundary check
	lexRel, err := filepath.Rel(canonicalWorkDir, target)
	if err != nil || lexRel == ".." || strings.HasPrefix(lexRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path outside workspace")
	}

	// Block .git paths
	if isGitPath(filepath.ToSlash(lexRel)) {
		return "", fmt.Errorf(".git paths are restricted")
	}

	// Evaluate symlinks
	fi, err := os.Lstat(target)
	if err == nil {
		// Target exists
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return "", fmt.Errorf("failed to resolve symlink: %w", err)
		}
		rel, err := filepath.Rel(canonicalWorkDir, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path outside workspace")
		}
		if isGitPath(filepath.ToSlash(rel)) {
			return "", fmt.Errorf(".git paths are restricted")
		}
		return resolved, nil
	}

	// Target does not exist - verify if target itself is a broken symlink
	if fi != nil && (fi.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("broken or inaccessible symlink")
	}

	// Find the lowest existing ancestor directory and verify it doesn't escape workspace
	curr := target
	var missingParts []string
	for {
		parent := filepath.Dir(curr)
		missingParts = append([]string{filepath.Base(curr)}, missingParts...)
		if parent == curr {
			break
		}
		if _, statErr := os.Lstat(parent); statErr == nil {
			evalParent, evalErr := filepath.EvalSymlinks(parent)
			if evalErr != nil {
				return "", fmt.Errorf("failed to resolve parent directory: %w", evalErr)
			}
			rel, relErr := filepath.Rel(canonicalWorkDir, evalParent)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("path outside workspace")
			}
			if isGitPath(filepath.ToSlash(rel)) {
				return "", fmt.Errorf(".git paths are restricted")
			}

			resolved := evalParent
			for _, part := range missingParts {
				resolved = filepath.Join(resolved, part)
			}

			relFinal, relErr := filepath.Rel(canonicalWorkDir, resolved)
			if relErr != nil || relFinal == ".." || strings.HasPrefix(relFinal, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("path outside workspace")
			}
			if isGitPath(filepath.ToSlash(relFinal)) {
				return "", fmt.Errorf(".git paths are restricted")
			}
			return resolved, nil
		}
		curr = parent
	}

	return "", fmt.Errorf("workspace directory does not exist")
}

func isGitPath(slashRel string) bool {
	lower := strings.ToLower(slashRel)
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
