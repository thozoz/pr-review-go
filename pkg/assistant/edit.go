// Package assistant gated edit loop (D-27/D-30/D-39).
//
// RunGatedEditLoop is the ONLY writable file path in the system. It executes
// untrusted LLM-directed file edits strictly inside the sealed snapshot work
// copy (the workDir handed in, produced by PrepareSnapshot), never on the host
// workspace. Every write passes lexical pre-checks, os.OpenRoot confinement,
// and a post-open handle verification, and is debited against EditCaps.
//
// This file intentionally has no transport, no git invocation, and no secret
// material of any kind.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Default edit-loop bounds (D-39).
const (
	// DefaultEditMaxFiles caps the number of distinct files per loop run.
	DefaultEditMaxFiles = 10
	// DefaultEditMaxTotalLines caps the total written lines per loop run.
	DefaultEditMaxTotalLines = 500
	// DefaultEditMaxTotalBytes caps the total written bytes per loop run (1 MiB).
	DefaultEditMaxTotalBytes = 1048576
	// MaxGatedEditTurns bounds the LLM turn loop.
	MaxGatedEditTurns = 8
	// MaxGatedEditViolations aborts the loop after repeated forbidden actions.
	MaxGatedEditViolations = 3
)

// ErrGatedEditViolationsExceeded aborts the loop after the model repeatedly
// requests forbidden mutation actions.
var ErrGatedEditViolationsExceeded = errors.New("gated edit loop: model exceeded violation limit")

// EditCaps bounds a single RunGatedEditLoop run (D-39).
type EditCaps struct {
	// MaxFiles caps distinct files written. Zero means DefaultEditMaxFiles.
	MaxFiles int
	// MaxTotalLines caps total lines written. Zero means DefaultEditMaxTotalLines.
	MaxTotalLines int
	// MaxTotalBytes caps total bytes written. Zero means DefaultEditMaxTotalBytes.
	MaxTotalBytes int
}

// DefaultEditCaps returns the plan-mandated bounds: 10 files, 500 lines, 1 MiB.
func DefaultEditCaps() EditCaps {
	return EditCaps{
		MaxFiles:      DefaultEditMaxFiles,
		MaxTotalLines: DefaultEditMaxTotalLines,
		MaxTotalBytes: DefaultEditMaxTotalBytes,
	}
}

func normalizeEditCaps(caps EditCaps) EditCaps {
	if caps.MaxFiles <= 0 {
		caps.MaxFiles = DefaultEditMaxFiles
	}
	if caps.MaxTotalLines <= 0 {
		caps.MaxTotalLines = DefaultEditMaxTotalLines
	}
	if caps.MaxTotalBytes <= 0 {
		caps.MaxTotalBytes = DefaultEditMaxTotalBytes
	}
	return caps
}

// EditResult reports what the gated loop changed inside the work copy.
type EditResult struct {
	// FilesChanged lists work-copy-relative paths written, in write order.
	FilesChanged []string
	// TotalLines counts written lines across all writes.
	TotalLines int
	// TotalBytes counts written bytes across all writes.
	TotalBytes int
	// OmissionNotice names skipped paths when a cap stopped the run.
	OmissionNotice string
	// Truncated is true when caps stopped the run before the model finished.
	Truncated bool
}

// GatedEditLLM is the minimal LLM surface the gated loop needs. *llm.Client
// satisfies it; tests inject scripted fakes.
type GatedEditLLM interface {
	ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// gatedEditSystemPrompt is a SEPARATE prompt from the read-only assistant and
// reviewer prompts: write_file is enabled under jail, while command execution
// and push transport stay refused.
const gatedEditSystemPrompt = `You are a code editing agent operating ONLY inside a sealed snapshot work copy.
You apply a single requested improvement to the files in the work copy.

AVAILABLE ACTIONS:
1. "read_file": Read a file inside the work copy. ({"action": "read_file", "path": "path/to/file"})
2. "list_files": List files in a directory inside the work copy. ({"action": "list_files", "path": "."})
3. "write_file": Create or overwrite a file INSIDE the work copy only. ({"action": "write_file", "path": "path/to/file", "content": "full file content"})
4. "answer": Finish and report what was changed. ({"action": "answer", "final_text": "summary of edits..."})

STRICT RULES:
- Every path must be relative to the work copy root (e.g. "pkg/foo/bar.go").
- Absolute paths, parent-directory escapes (".."), and .git paths are rejected.
- "run_command", "commit_and_push", "exec", "shell", and "push" are forbidden and will be refused. You cannot run commands or push; just edit files, then answer.
- Keep edits small and focused. You have up to 8 turns before the loop stops.
- Output MUST be a single strict JSON object matching {"action": "...", ...}.
- Never include markdown codeblocks surrounding your JSON tool calls.`

// gatedToolCall is the gated loop's wire format. It carries a content payload
// for write_file, which the read-only ToolCallRequest cannot express.
type gatedToolCall struct {
	Action    string `json:"action"`
	Path      string `json:"path,omitempty"`
	Content   string `json:"content,omitempty"`
	FinalText string `json:"final_text,omitempty"`
}

func parseGatedToolCall(raw string) (*gatedToolCall, error) {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no json object found")
	}
	trimmed = trimmed[start : end+1]

	var req gatedToolCall
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// isGatedRefusedAction is the gated allow-list's refusal side: command
// execution and push transport are never permitted in the edit loop.
// It is deliberately independent of the ungated reviewer's mutation helper,
// which must stay byte-identical for the read-only paths.
func isGatedRefusedAction(act string) bool {
	switch strings.ToLower(strings.TrimSpace(act)) {
	case "run_command", "commit_and_push", "exec", "shell", "push", "commit", "bash", "delete_file":
		return true
	}
	return false
}

// RunGatedEditLoop applies one LLM-directed improvement inside workDir, the
// sealed snapshot work copy. It returns the applied-change summary; the caller
// assembles any host-side push from the returned byte state (plan 05-04).
func RunGatedEditLoop(ctx context.Context, llmClient GatedEditLLM, workDir string, instruction string, stickyContext string, caps EditCaps) (EditResult, error) {
	var result EditResult
	if llmClient == nil {
		return result, fmt.Errorf("gated edit loop: nil LLM client")
	}
	caps = normalizeEditCaps(caps)

	canonicalWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return result, fmt.Errorf("gated edit loop: invalid work copy: %w", err)
	}
	if evaled, err := filepath.EvalSymlinks(canonicalWorkDir); err == nil {
		canonicalWorkDir = evaled
	}
	fi, err := os.Stat(canonicalWorkDir)
	if err != nil || !fi.IsDir() {
		return result, fmt.Errorf("gated edit loop: work copy is not a directory")
	}

	history := fmt.Sprintf("Context (diff text plus instructions):\n%s\n\nRequested improvement:\n%s\n",
		stickyContext, instruction)

	seen := make(map[string]bool)
	violations := 0

	for turn := 0; turn < MaxGatedEditTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		rawResponse, err := llmClient.ChatCompletion(ctx, gatedEditSystemPrompt, history)
		if err != nil {
			return result, fmt.Errorf("gated edit loop: llm step failed: %w", err)
		}

		step, err := parseGatedToolCall(rawResponse)
		if err != nil {
			// Plain text or unparseable output ends the loop as a final answer.
			return result, nil
		}

		if step.Action == "" || step.Action == "answer" {
			return result, nil
		}

		if isGatedRefusedAction(step.Action) {
			violations++
			refusal := fmt.Sprintf("Error: %s is disabled: the gated edit loop permits file edits only; command execution and push transport are not available.", step.Action)
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action (write_file, read_file, list_files, or answer):",
				step.Action, refusal)
			if violations >= MaxGatedEditViolations {
				return result, fmt.Errorf("%w: refused %d forbidden attempts (%s)", ErrGatedEditViolationsExceeded, violations, step.Action)
			}
			continue
		}

		switch step.Action {
		case "read_file":
			content, rErr := readWorkspaceFile(canonicalWorkDir, step.Path)
			if rErr != nil {
				history += fmt.Sprintf("\nAction requested: read_file (path: %s)\nTool Result:\nError: %v\nNext action:", step.Path, rErr)
			} else {
				history += fmt.Sprintf("\nAction requested: read_file (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", step.Path, content)
			}
		case "list_files":
			p := step.Path
			if p == "" {
				p = "."
			}
			listing, lErr := listWorkspaceFiles(canonicalWorkDir, p)
			if lErr != nil {
				history += fmt.Sprintf("\nAction requested: list_files (path: %s)\nTool Result:\nError: %v\nNext action:", p, lErr)
			} else {
				history += fmt.Sprintf("\nAction requested: list_files (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", p, listing)
			}
		case "write_file":
			newLines := countGatedLines(step.Content)
			newBytes := len(step.Content)
			slashPath := filepath.ToSlash(filepath.Clean(step.Path))

			capHit := ""
			switch {
			case !seen[slashPath] && len(seen) >= caps.MaxFiles:
				capHit = fmt.Sprintf("file cap exceeded (max %d files)", caps.MaxFiles)
			case result.TotalLines+newLines > caps.MaxTotalLines:
				capHit = fmt.Sprintf("line cap exceeded (max %d lines)", caps.MaxTotalLines)
			case result.TotalBytes+newBytes > caps.MaxTotalBytes:
				capHit = fmt.Sprintf("byte cap exceeded (max %d bytes)", caps.MaxTotalBytes)
			}
			if capHit != "" {
				result.Truncated = true
				result.OmissionNotice = fmt.Sprintf("%s: omitted %s and all further edits", capHit, step.Path)
				return result, nil
			}

			if err := gatedWriteFile(canonicalWorkDir, step.Path, step.Content); err != nil {
				return result, err
			}
			if !seen[slashPath] {
				seen[slashPath] = true
				result.FilesChanged = append(result.FilesChanged, slashPath)
			}
			result.TotalLines += newLines
			result.TotalBytes += newBytes
			history += fmt.Sprintf("\nAction requested: write_file (path: %s)\nTool Result:\nWrote %d bytes.\nNext action (or answer):", step.Path, newBytes)
		default:
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\nUnknown action: %s. Use read_file, list_files, write_file, or answer.\nNext action:", step.Action, step.Action)
		}
	}

	result.Truncated = true
	if result.OmissionNotice == "" {
		result.OmissionNotice = fmt.Sprintf("turn budget exhausted after %d turns; further edits omitted", MaxGatedEditTurns)
	}
	return result, nil
}

func countGatedLines(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

// gatedWriteFile writes content to a path jailed inside canonicalWorkDir. It
// mirrors the read-side lexical pre-checks, confines resolution with
// os.OpenRoot, verifies the open handle, and refuses symlinks and
// non-regular files.
func gatedWriteFile(canonicalWorkDir, userPath, content string) error {
	if userPath == "" {
		return fmt.Errorf("empty path")
	}

	// Lexical pre-checks (mirroring the read side).
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

	// Create missing parent directories inside the jail.
	if dir := filepath.Dir(cleanRel); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("access denied (%w)", err)
		}
	}

	f, err := root.OpenFile(cleanRel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("access denied (%w)", err)
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}
	if finfo.IsDir() {
		return fmt.Errorf("cannot write directory as file")
	}
	if finfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("access denied: not a regular file")
	}
	if !finfo.Mode().IsRegular() {
		return fmt.Errorf("access denied: not a regular file")
	}

	// Post-open handle verification: the kernel-confirmed target must stay
	// inside the work copy and outside restricted locations.
	if err := verifyFileHandle(f, canonicalWorkDir); err != nil {
		return fmt.Errorf("access denied: %w", err)
	}

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("error writing file: %w", err)
	}
	return nil
}
