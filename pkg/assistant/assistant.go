package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
		gh:      github.NewClient(cfg.GitHubToken),
		sandbox: sandbox.NewRunner(2 * time.Minute),
		llm:     llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

type ToolCallRequest struct {
	Action    string `json:"action"`     // "read_file", "write_file", "list_files", "run_command", "commit_and_push", "answer"
	Path      string `json:"path"`       // For read_file, write_file, or list_files
	Content   string `json:"content"`    // For write_file
	Command   string `json:"command"`    // For run_command
	CommitMsg string `json:"commit_msg"` // For commit_and_push
	FinalText string `json:"final_text"` // For answer
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

	systemPrompt := `You are an elite, fully autonomous AI Coding Agent operating inside an isolated Git Pull Request sandbox environment (equivalent to GitHub Copilot Cloud Agent).
You can inspect code, run tests, resolve merge conflicts, fix bugs, add new code/tests, and commit & push changes back to the PR branch.

AVAILABLE ACTIONS:
1. "read_file": Read content of a file. ({"action": "read_file", "path": "path/to/file"})
2. "write_file": Write or update file content. ({"action": "write_file", "path": "path/to/file", "content": "..."})
3. "list_files": List files in a directory. ({"action": "list_files", "path": "."})
4. "run_command": Execute any shell command in the project root. ({"action": "run_command", "command": "go test -v ./..." or "git status"})
5. "commit_and_push": Commit modified files and push to PR branch. ({"action": "commit_and_push", "commit_msg": "fix: resolve merge conflicts in auth.go"})
6. "answer": Deliver your final response to the user. ({"action": "answer", "final_text": "Markdown summary of what was done or answered..."})

GENERAL AGENT RULES:
- If asked to fix a bug or resolve merge conflicts:
  1. Inspect the relevant files or run git commands.
  2. Apply the fix using write_file.
  3. ALWAYS verify your changes by executing compilers/test suites using run_command!
  4. Once tests pass, commit and push your changes using commit_and_push.
  5. Provide your final explanation via answer.
- If asked a question or diagnosis without code modifications, inspect code/tests and deliver your final answer.
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
		history += fmt.Sprintf("\nAction executed: %s (path: %s, cmd: %s)\nTool Result:\n```\n%s\n```\nNext action (or provide final answer):",
			step.Action, step.Path, step.Command, toolOutput)
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
		cleanPath := filepath.Clean(step.Path)
		target := filepath.Join(workDir, cleanPath)
		if !strings.HasPrefix(target, workDir) {
			return "Error: Access denied (path outside workspace)."
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
		cleanPath := filepath.Clean(step.Path)
		target := filepath.Join(workDir, cleanPath)
		if !strings.HasPrefix(target, workDir) {
			return "Error: Access denied (path outside workspace)."
		}
		// Create directory if it doesn't exist
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return fmt.Sprintf("Error creating parent directories: %v", err)
		}
		if err := os.WriteFile(target, []byte(step.Content), 0644); err != nil {
			return fmt.Sprintf("Error writing file: %v", err)
		}
		return fmt.Sprintf("Success: File %s updated (%d bytes written).", step.Path, len(step.Content))

	case "list_files":
		cleanPath := filepath.Clean(step.Path)
		target := filepath.Join(workDir, cleanPath)
		if !strings.HasPrefix(target, workDir) {
			return "Error: Access denied."
		}
		var files []string
		_ = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
			if err != nil || len(files) > 100 {
				return nil
			}
			rel, _ := filepath.Rel(workDir, p)
			if rel != "." && !strings.HasPrefix(rel, ".git") {
				files = append(files, rel)
			}
			return nil
		})
		return strings.Join(files, "\n")

	case "run_command":
		cmdCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, "sh", "-c", step.Command)
		cmd.Dir = workDir

		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf

		err := cmd.Run()
		outStr := outBuf.String()
		errStr := errBuf.String()

		res := fmt.Sprintf("STDOUT:\n%s\nSTDERR:\n%s", outStr, errStr)
		if err != nil {
			res += fmt.Sprintf("\nExit Code / Error: %v", err)
		}
		if len(res) > 40000 {
			res = res[:40000] + "\n...[output truncated]..."
		}
		return res

	case "commit_and_push":
		msg := strings.TrimSpace(step.CommitMsg)
		if msg == "" {
			msg = "fix: apply changes suggested by AI assistant"
		}

		// Configure git user inside sandbox
		_ = exec.Command("git", "-C", workDir, "config", "user.name", "pr-review-go[bot]").Run()
		_ = exec.Command("git", "-C", workDir, "config", "user.email", "pr-review-go[bot]@users.noreply.github.com").Run()

		// Git add
		addCmd := exec.Command("git", "-C", workDir, "add", "-A")
		if err := addCmd.Run(); err != nil {
			return fmt.Sprintf("Error staging files: %v", err)
		}

		// Git commit
		commitCmd := exec.Command("git", "-C", workDir, "commit", "-m", msg)
		var commitErr bytes.Buffer
		commitCmd.Stderr = &commitErr
		if err := commitCmd.Run(); err != nil {
			return fmt.Sprintf("Git commit failed (nothing to commit or error): %v - %s", err, commitErr.String())
		}

		// Git push
		pushCmd := exec.CommandContext(ctx, "git", "-C", workDir, "push", "origin", "HEAD")
		var pushErr bytes.Buffer
		pushCmd.Stderr = &pushErr
		if err := pushCmd.Run(); err != nil {
			return fmt.Sprintf("Git push failed: %v - %s", err, pushErr.String())
		}

		return fmt.Sprintf("Success: Commit '%s' created and successfully pushed to PR branch.", msg)

	default:
		return fmt.Sprintf("Unknown action: %s", step.Action)
	}
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
