package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Constants governing bounded edit loop execution and safety thresholds.
const (
	MaxEditTurns        = 8
	MaxEditViolations   = 3
	DefaultMaxFiles     = 10
	DefaultMaxTotalLines = 500
	DefaultMaxTotalBytes = 1048576 // 1 MiB
)

// LLMClient abstracts conversational completion calls for the edit loop.
type LLMClient interface {
	ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// EditCaps defines the operational quotas for gated file modifications.
type EditCaps struct {
	MaxFiles      int
	MaxTotalLines int
	MaxTotalBytes int
}

// DefaultEditCaps returns standard operational caps: 10 files, 500 lines, 1048576 bytes.
func DefaultEditCaps() EditCaps {
	return EditCaps{
		MaxFiles:      DefaultMaxFiles,
		MaxTotalLines: DefaultMaxTotalLines,
		MaxTotalBytes: DefaultMaxTotalBytes,
	}
}

// EditResult captures the outcome and statistics of a completed or truncated edit session.
type EditResult struct {
	FilesChanged   []string
	TotalLines     int
	TotalBytes     int
	OmissionNotice string
	Truncated      bool
}

// EditLoop encapsulates configuration for running an isolated gated edit session.
type EditLoop struct {
	Caps EditCaps
}

// NewEditLoop instantiates an EditLoop with explicit caps.
func NewEditLoop(caps EditCaps) *EditLoop {
	return &EditLoop{Caps: caps}
}

// Run executes the edit loop within the provided working directory.
func (l *EditLoop) Run(ctx context.Context, llmClient LLMClient, workDir, instruction, stickyContext string) (EditResult, error) {
	return RunGatedEditLoop(ctx, llmClient, workDir, instruction, stickyContext, l.Caps)
}

type editToolCall struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	FinalText string `json:"final_text"`
}

const gatedEditSystemPrompt = `You are an AI Coding Assistant operating on an isolated Git work copy.
You can inspect files and make edits to implement the requested instruction.

AVAILABLE ACTIONS:
1. "write_file": Write content to a file. ({"action": "write_file", "path": "path/to/file", "content": "file content..."})
2. "read_file": Read content of a file. ({"action": "read_file", "path": "path/to/file"})
3. "list_files": List files in a directory. ({"action": "list_files", "path": "."})
4. "answer": Deliver your final summary when editing is complete. ({"action": "answer", "final_text": "Summary of changes made..."})

GENERAL AGENT RULES:
- Inspect files and write changes directly to fulfill the user's instruction.
- You can take up to 8 iterative turns to complete the edits.
- Direct command execution (run_command, exec, shell) and git push operations are strictly prohibited.
- Output MUST be a single strict JSON object matching: {"action": "...", ...}
- Never include markdown codeblocks surrounding your JSON tool calls.`

// RunGatedEditLoop executes an isolated, bounded turn loop against workDir.
// It strictly confines all file mutations to workDir under os.OpenRoot and verifyFileHandle,
// enforces EditCaps limits, and terminates on unauthorized command or git mutation attempts.
func RunGatedEditLoop(ctx context.Context, llmClient LLMClient, workDir string, instruction string, stickyContext string, caps EditCaps) (EditResult, error) {
	if caps.MaxFiles <= 0 {
		caps.MaxFiles = DefaultMaxFiles
	}
	if caps.MaxTotalLines <= 0 {
		caps.MaxTotalLines = DefaultMaxTotalLines
	}
	if caps.MaxTotalBytes <= 0 {
		caps.MaxTotalBytes = DefaultMaxTotalBytes
	}

	result := EditResult{
		FilesChanged: []string{},
	}
	filesChangedMap := make(map[string]bool)

	history := fmt.Sprintf("User Instruction:\n%s\n", instruction)
	if stickyContext != "" {
		history += fmt.Sprintf("\nContext:\n%s\n", stickyContext)
	}
	history += "\nBegin by inspecting or editing the necessary files.\n"

	violations := 0

	for turn := 0; turn < MaxEditTurns; turn++ {
		rawResponse, err := llmClient.ChatCompletion(ctx, gatedEditSystemPrompt, history)
		if err != nil {
			return result, fmt.Errorf("llm edit step failed: %w", err)
		}

		steps, err := parseEditToolCalls(rawResponse)
		if err != nil {
			// If not parseable as JSON, treat turn as final or break
			break
		}

		shouldConclude := false
		for _, step := range steps {
			if isForbiddenEditAction(step.Action) {
				violations++
				refusalMsg := fmt.Sprintf("Error: %s is disabled: command execution and git operations are not permitted in the edit loop.", step.Action)
				history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action (or conclude via action='answer'):", step.Action, refusalMsg)
				if violations >= MaxEditViolations {
					return result, fmt.Errorf("edit loop aborted: exceeded maximum violations (%d) attempting unauthorized action %s", MaxEditViolations, step.Action)
				}
				continue
			}

			switch step.Action {
			case "answer":
				shouldConclude = true
				break

			case "read_file":
				content, err := readWorkspaceFile(workDir, step.Path)
				var toolOutput string
				if err != nil {
					toolOutput = fmt.Sprintf("Error reading file: %v", err)
				} else {
					toolOutput = content
				}
				history += fmt.Sprintf("\nAction requested: read_file (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", step.Path, toolOutput)

			case "list_files":
				p := step.Path
				if p == "" {
					p = "."
				}
				listing, err := listWorkspaceFiles(workDir, p)
				var toolOutput string
				if err != nil {
					toolOutput = fmt.Sprintf("Error listing files: %v", err)
				} else {
					toolOutput = listing
				}
				history += fmt.Sprintf("\nAction requested: list_files (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", p, toolOutput)

			case "write_file":
				cleanRel := filepath.Clean(step.Path)
				slashRel := filepath.ToSlash(cleanRel)

				// Pre-validate lexical path sanity before accounting caps
				if step.Path == "" || strings.ContainsRune(step.Path, 0) || filepath.IsAbs(step.Path) ||
					filepath.VolumeName(step.Path) != "" || strings.HasPrefix(step.Path, "/") ||
					strings.HasPrefix(step.Path, "\\") || slashRel == ".." || strings.HasPrefix(slashRel, "../") ||
					isGitPath(slashRel) {
					writeErr := writeWorkspaceFile(workDir, step.Path, step.Content)
					toolOutput := fmt.Sprintf("Error writing file: %v", writeErr)
					history += fmt.Sprintf("\nAction requested: write_file (path: %s)\nTool Result:\n%s\nNext action:", step.Path, toolOutput)
					continue
				}

				contentBytes := len([]byte(step.Content))
				contentLines := countLines(step.Content)

				// Evaluate caps
				isNewFile := !filesChangedMap[slashRel]
				if isNewFile && len(result.FilesChanged)+1 > caps.MaxFiles {
					result.Truncated = true
					result.OmissionNotice = fmt.Sprintf("Edit caps exceeded: maximum files limit (%d) reached; skipped %s", caps.MaxFiles, slashRel)
					return result, nil
				}

				if result.TotalBytes+contentBytes > caps.MaxTotalBytes {
					result.Truncated = true
					result.OmissionNotice = fmt.Sprintf("Edit caps exceeded: maximum bytes limit (%d) reached; skipped %s (%d bytes)", caps.MaxTotalBytes, slashRel, contentBytes)
					return result, nil
				}

				if result.TotalLines+contentLines > caps.MaxTotalLines {
					result.Truncated = true
					result.OmissionNotice = fmt.Sprintf("Edit caps exceeded: maximum lines limit (%d) reached; skipped %s (%d lines)", caps.MaxTotalLines, slashRel, contentLines)
					return result, nil
				}

				writeErr := writeWorkspaceFile(workDir, step.Path, step.Content)
				if writeErr != nil {
					toolOutput := fmt.Sprintf("Error writing file: %v", writeErr)
					history += fmt.Sprintf("\nAction requested: write_file (path: %s)\nTool Result:\n%s\nNext action:", step.Path, toolOutput)
				} else {
					if isNewFile {
						filesChangedMap[slashRel] = true
						result.FilesChanged = append(result.FilesChanged, slashRel)
					}
					result.TotalBytes += contentBytes
					result.TotalLines += contentLines
					toolOutput := fmt.Sprintf("Successfully wrote %d bytes (%d lines) to %s", contentBytes, contentLines, slashRel)
					history += fmt.Sprintf("\nAction requested: write_file (path: %s)\nTool Result:\n%s\nNext action:", step.Path, toolOutput)
				}

			default:
				violations++
				refusalMsg := fmt.Sprintf("Error: unauthorized action %s is not permitted in the edit loop.", step.Action)
				history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action:", step.Action, refusalMsg)
				if violations >= MaxEditViolations {
					return result, fmt.Errorf("edit loop aborted: exceeded maximum violations (%d) attempting unauthorized action %s", MaxEditViolations, step.Action)
				}
			}
		}

		if shouldConclude {
			return result, nil
		}
	}

	return result, nil
}

func countLines(s string) int {
	if len(s) == 0 {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func isForbiddenEditAction(act string) bool {
	lower := strings.ToLower(strings.TrimSpace(act))
	switch lower {
	case "run_command", "commit_and_push", "exec", "shell", "push", "bash", "commit", "delete_file":
		return true
	}
	if strings.Contains(lower, "command") || strings.Contains(lower, "exec") ||
		strings.Contains(lower, "shell") || strings.Contains(lower, "push") ||
		strings.Contains(lower, "commit") {
		return true
	}
	return false
}

func parseEditToolCalls(raw string) ([]*editToolCall, error) {
	trimmed := strings.TrimSpace(raw)

	// Check for JSON array of tool calls
	startArr := strings.Index(trimmed, "[")
	endArr := strings.LastIndex(trimmed, "]")
	if startArr >= 0 && endArr > startArr {
		var reqs []*editToolCall
		if err := json.Unmarshal([]byte(trimmed[startArr:endArr+1]), &reqs); err == nil && len(reqs) > 0 {
			return reqs, nil
		}
	}

	// Check for single JSON object
	startObj := strings.Index(trimmed, "{")
	endObj := strings.LastIndex(trimmed, "}")
	if startObj >= 0 && endObj > startObj {
		var req editToolCall
		if err := json.Unmarshal([]byte(trimmed[startObj:endObj+1]), &req); err == nil {
			return []*editToolCall{&req}, nil
		}
	}

	return nil, fmt.Errorf("no valid json tool call found")
}

// writeWorkspaceFile safely opens and writes a regular file strictly inside workDir.
// It verifies directory hierarchy, enforces os.OpenRoot containment, rejects symlinks,
// and invokes verifyFileHandle post-open verification.
func writeWorkspaceFile(workDir, userPath, content string) error {
	if userPath == "" {
		return fmt.Errorf("empty path")
	}

	canonicalWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("invalid workspace: %w", err)
	}
	if evaled, err := filepath.EvalSymlinks(canonicalWorkDir); err == nil {
		canonicalWorkDir = evaled
	}

	// Lexical pre-checks matching assistant.go readWorkspaceFile
	if strings.ContainsRune(userPath, 0) {
		return fmt.Errorf("invalid path: contains null byte")
	}
	if filepath.IsAbs(userPath) || filepath.VolumeName(userPath) != "" ||
		strings.HasPrefix(userPath, "/") || strings.HasPrefix(userPath, "\\") {
		return fmt.Errorf("absolute paths are not permitted")
	}

	cleanRel := filepath.Clean(userPath)
	slashRel := filepath.ToSlash(cleanRel)
	if slashRel == "." {
		return fmt.Errorf("cannot write directory as file")
	}
	if slashRel == ".." || strings.HasPrefix(slashRel, "../") {
		return fmt.Errorf("path outside workspace")
	}
	if isGitPath(slashRel) {
		return fmt.Errorf(".git paths are restricted")
	}

	root, err := os.OpenRoot(canonicalWorkDir)
	if err != nil {
		return fmt.Errorf("failed to open workspace: %w", err)
	}
	defer root.Close()

	// Refuse symlinks in any directory component leading up to the file
	dirPart := filepath.Dir(cleanRel)
	if dirPart != "." && dirPart != "" {
		slashDir := filepath.ToSlash(dirPart)
		parts := strings.Split(slashDir, "/")
		curr := ""
		for _, part := range parts {
			if curr == "" {
				curr = part
			} else {
				curr = curr + "/" + part
			}
			if fi, err := root.Lstat(curr); err == nil {
				if fi.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("access denied: symlinks are not permitted")
				}
				if !fi.IsDir() {
					return fmt.Errorf("access denied: path component is not a directory")
				}
			}
		}
		if err := root.MkdirAll(dirPart, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Refuse symlinks or non-regular files if target already exists
	if fi, err := root.Lstat(cleanRel); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("access denied: symlinks are not permitted")
		}
		if fi.IsDir() {
			return fmt.Errorf("cannot write directory as file")
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("access denied: not a regular file")
		}
	}

	// Open file strictly relative to root with O_CREATE|os.O_WRONLY|os.O_TRUNC
	f, err := root.OpenFile(cleanRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("access denied (%w)", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("cannot write directory as file")
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("access denied: not a regular file")
	}

	// Post-open handle verification
	if err := verifyFileHandle(f, canonicalWorkDir); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("failed to write file content: %w", err)
	}

	return nil
}
