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
	// MaxGatedEditTurns bounds the LLM turn loop (D-01 inner budget: 15 rounds
	// per outer attempt; worst case 15 inner x 3 outer editVerifyMaxAttempts =
	// 45 LLM calls, bounded by RequestGate plus the 10-minute job timeout).
	MaxGatedEditTurns = 15
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
3. "write_file": Create a NEW file INSIDE the work copy only. ({"action": "write_file", "path": "path/to/file", "content": "full file content"}) Do NOT use write_file to overwrite an existing file; use str_replace for edits to existing files.
4. "str_replace": Replace one exact occurrence of old_str with new_str in an existing file INSIDE the work copy. old_str must match EXACTLY ONCE; zero or multiple matches fail with no write. ({"action": "str_replace", "path": "path/to/file", "old_str": "exact text", "new_str": "replacement"})
5. "run_command": Run one allow-listed build/test command INSIDE the work copy (fixed argv only, no shell; binary must be one of go, npm, pnpm, yarn, pytest, python3, python, cargo, mvn, gradle, make). ({"action": "run_command", "command": ["go", "test", "./..."]})
6. "grep": Search file contents inside the work copy (read-only). ({"action": "grep", "pattern": "text", "path": "."})
7. "glob": Find files by name inside the work copy (read-only). ({"action": "glob", "pattern": "*.go", "path": "."})
8. "answer": Finish and report what was changed. ({"action": "answer", "final_text": "summary of edits..."})

STRICT RULES:
- Every path must be relative to the work copy root (e.g. "pkg/foo/bar.go").
- Absolute paths, parent-directory escapes (".."), and .git paths are rejected.
- "commit_and_push", "exec", "shell", "push", "commit", "bash", and "delete_file" are forbidden and will be refused. Only the allow-listed run_command form above may execute anything; everything else is file edits, then answer.
- Keep edits small and focused. You have up to 15 turns before the loop stops.
- Output MUST be a single strict JSON object matching {"action": "...", ...}.
- Never include markdown codeblocks surrounding your JSON tool calls.`

// gatedToolCall is the gated loop's wire format. It carries a content payload
// for write_file, which the read-only ToolCallRequest cannot express, plus
// patch (old_str/new_str) fields for the Phase 6 str_replace tool.
type gatedToolCall struct {
	Action    string   `json:"action"`
	Path      string   `json:"path,omitempty"`
	Content   string   `json:"content,omitempty"`
	FinalText string   `json:"final_text,omitempty"`
	OldStr    string   `json:"old_str,omitempty"`
	NewStr    string   `json:"new_str,omitempty"`
	Command   []string `json:"command,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
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
	case "commit_and_push", "exec", "shell", "push", "commit", "bash", "delete_file":
		return true
	}
	return false
}

// gatedRunCommandBackend executes an allow-listed argv with cwd pinned to the
// work copy. It is nil by default: with no backend assigned the gated loop
// reports tool-unavailable for well-formed commands (a later plan assigns the
// direct/podman-backed implementation). This plan asserts the argv gate, not
// the backend.
var gatedRunCommandBackend func(ctx context.Context, workDir string, argv []string) (stdout string, stderr string, exitCode int, err error)

// gatedRunCommandAllowlist pins the runnable binary head to the build/test
// matrix from pkg/sandbox/direct.go detectDirectProject. No shell, no
// network fetchers, no version-control or push binaries.
var gatedRunCommandAllowlist = map[string]bool{
	"go": true, "npm": true, "pnpm": true, "yarn": true,
	"pytest": true, "python3": true, "python": true,
	"cargo": true, "mvn": true, "gradle": true, "make": true,
}

// validateGatedRunCommand enforces the fixed-argv gate: 1..8 elements, an
// allow-listed relative binary head, no shell metacharacters, no absolute
// paths, and no parent-directory traversal in any element.
func validateGatedRunCommand(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command argv")
	}
	if len(argv) > 8 {
		return fmt.Errorf("command argv exceeds 8 elements")
	}
	head := argv[0]
	if head == "" {
		return fmt.Errorf("empty command binary")
	}
	if !gatedRunCommandAllowlist[head] {
		return fmt.Errorf("binary %q is not in the run_command allow-list", head)
	}
	for i, el := range argv {
		if strings.ContainsAny(el, ";&|$`\\\n\r") {
			return fmt.Errorf("shell metacharacter in argv element %d", i)
		}
		if filepath.IsAbs(el) || filepath.VolumeName(el) != "" ||
			strings.HasPrefix(el, "/") || strings.HasPrefix(el, "\\") {
			return fmt.Errorf("absolute path in argv element %d", i)
		}
		if el == ".." || strings.HasPrefix(el, "../") || strings.HasPrefix(el, `..\`) {
			return fmt.Errorf("parent-directory traversal in argv element %d", i)
		}
	}
	return nil
}

// gatedGrepOutputCap bounds grep observations fed back into the loop.
const gatedGrepOutputCap = 6000

// gatedGrepFiles line-matches pattern with strings.Contains across every
// readable file under userPath inside the jail. It is read-only: files are
// opened through the same OpenRoot plus handle-verified read path as
// read_file, and nothing is written.
func gatedGrepFiles(canonicalWorkDir, userPath, pattern string) (string, error) {
	p := userPath
	if p == "" {
		p = "."
	}
	listing, err := listWorkspaceFiles(canonicalWorkDir, p)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	truncated := false
	for _, rel := range strings.Split(listing, "\n") {
		if rel == "" {
			continue
		}
		slashRel := filepath.ToSlash(rel)
		content, err := readWorkspaceFile(canonicalWorkDir, slashRel)
		if err != nil {
			continue // directories and unreadable entries are skipped
		}
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(line, pattern) {
				fmt.Fprintf(&sb, "%s:%d:%s\n", slashRel, i+1, line)
				if sb.Len() > gatedGrepOutputCap {
					truncated = true
					break
				}
			}
		}
		if truncated {
			break
		}
	}
	out := sb.String()
	if len(out) > gatedGrepOutputCap {
		out = out[:gatedGrepOutputCap]
	}
	if truncated {
		out += "\n...[truncated: grep output exceeds 6000 bytes]..."
	}
	return out, nil
}

// gatedGlobMaxEntries bounds glob observations fed back into the loop.
const gatedGlobMaxEntries = 200

// gatedGlobFiles matches pattern against file names under userPath inside the
// jail. Patterns without a path separator match base names; patterns with a
// separator match the full work-copy-relative slash path. Read-only: it only
// lists through the jailed listWorkspaceFiles path and writes nothing.
func gatedGlobFiles(canonicalWorkDir, userPath, pattern string) (string, error) {
	p := userPath
	if p == "" {
		p = "."
	}
	listing, err := listWorkspaceFiles(canonicalWorkDir, p)
	if err != nil {
		return "", err
	}
	hasSep := strings.Contains(pattern, "/") || strings.Contains(pattern, "\\")
	var matched []string
	for _, rel := range strings.Split(listing, "\n") {
		if rel == "" {
			continue
		}
		slashRel := filepath.ToSlash(rel)
		candidate := slashRel
		matchPattern := pattern
		if !hasSep {
			candidate = slashRel[strings.LastIndex(slashRel, "/")+1:]
		} else {
			// Compare in OS-native form so separators match on every platform.
			candidate = filepath.FromSlash(slashRel)
			matchPattern = filepath.FromSlash(pattern)
		}
		ok, mErr := filepath.Match(matchPattern, candidate)
		if mErr != nil {
			return "", fmt.Errorf("invalid glob pattern: %w", mErr)
		}
		if ok {
			matched = append(matched, slashRel)
			if len(matched) > gatedGlobMaxEntries {
				break
			}
		}
	}
	if len(matched) > gatedGlobMaxEntries {
		matched = matched[:gatedGlobMaxEntries]
		return strings.Join(matched, "\n") + "\n...[truncated: glob output exceeds 200 entries]...", nil
	}
	return strings.Join(matched, "\n"), nil
}

// History summarization bounds (D-01). The loop history grows every round;
// past the threshold older rounds compress while the latest tool results
// (always appended at the tail) survive verbatim.
const (
	// gatedHistorySummarizeThreshold triggers summarization of the loop history.
	gatedHistorySummarizeThreshold = 24000
	// gatedHistoryHeadBytes preserves the opening instruction context
	// (the Context opener plus requested improvement) verbatim.
	gatedHistoryHeadBytes = 2000
	// gatedHistoryTailBytes preserves the most recent rounds verbatim.
	gatedHistoryTailBytes = 12000
)

// summarizeGatedHistory bounds loop-history growth (D-01): histories at or
// below the summarize threshold pass through unchanged; longer histories keep
// the first gatedHistoryHeadBytes (original instruction context) plus a byte
// count marker plus the last keepTailBytes bytes verbatim. The marker is
// plain data, never a tool directive: cap-trip and violation state live in
// loop variables, never in the summarized text.
func summarizeGatedHistory(history string, keepTailBytes int) string {
	if len(history) <= gatedHistorySummarizeThreshold {
		return history
	}
	if keepTailBytes <= 0 {
		keepTailBytes = gatedHistoryTailBytes
	}
	if keepTailBytes > len(history) {
		keepTailBytes = len(history)
	}
	head := history
	if len(history) > gatedHistoryHeadBytes {
		head = history[:gatedHistoryHeadBytes]
	}
	tail := history[len(history)-keepTailBytes:]
	if dropped := len(history) - len(head) - len(tail); dropped <= 0 {
		return history
	} else {
		return head + fmt.Sprintf("\n...[summarized: %d earlier bytes compressed]...\n", dropped) + tail
	}
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

		// Summarized view only: the accumulated history keeps growing (cap
		// and violation state live in loop variables), but the model sees
		// older rounds compressed with the latest tool results verbatim.
		promptHistory := history
		if len(history) > gatedHistorySummarizeThreshold {
			promptHistory = summarizeGatedHistory(history, gatedHistoryTailBytes)
		}

		rawResponse, err := llmClient.ChatCompletion(ctx, gatedEditSystemPrompt, promptHistory)
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
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action (read_file, list_files, write_file, str_replace, run_command, grep, glob, or answer):",
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
		case "str_replace", "str_replace_editor":
			original, rErr := readWorkspaceFile(canonicalWorkDir, step.Path)
			if rErr != nil {
				history += fmt.Sprintf("\nAction requested: str_replace (path: %s)\nTool Result:\nError: %v\nNext action:", step.Path, rErr)
				break
			}
			if step.OldStr == "" {
				history += fmt.Sprintf("\nAction requested: str_replace (path: %s)\nTool Result:\nError: old_str must not be empty (exactly-once match required, found 0 matches); no bytes written.\nNext action:", step.Path)
				break
			}
			matches := strings.Count(original, step.OldStr)
			if matches != 1 {
				history += fmt.Sprintf("\nAction requested: str_replace (path: %s)\nTool Result:\nError: old_str must match exactly-once (found %d matches); no bytes written.\nNext action:", step.Path, matches)
				break
			}
			patched := strings.Replace(original, step.OldStr, step.NewStr, 1)
			newLines := countGatedLines(patched)
			newBytes := len(patched)
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

			if err := gatedWriteFile(canonicalWorkDir, step.Path, patched); err != nil {
				return result, err
			}
			if !seen[slashPath] {
				seen[slashPath] = true
				result.FilesChanged = append(result.FilesChanged, slashPath)
			}
			result.TotalLines += newLines
			result.TotalBytes += newBytes
			history += fmt.Sprintf("\nAction requested: str_replace (path: %s)\nTool Result:\nPatched 1 occurrence, wrote %d bytes.\nNext action (or answer):", step.Path, newBytes)
		case "run_command":
			if err := validateGatedRunCommand(step.Command); err != nil {
				violations++
				refusal := fmt.Sprintf("Error: %s is disabled: the gated edit loop permits file edits only; command execution and push transport are not available. (gate: %v)", step.Action, err)
				history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action:",
					step.Action, refusal)
				if violations >= MaxGatedEditViolations {
					return result, fmt.Errorf("%w: refused %d forbidden attempts (%s)", ErrGatedEditViolationsExceeded, violations, step.Action)
				}
				break
			}
			if gatedRunCommandBackend == nil {
				history += fmt.Sprintf("\nAction requested: run_command (command: %s)\nTool Result:\nError: tool-unavailable: no command backend is assigned; the allow-listed argv was accepted but not executed.\nNext action:", strings.Join(step.Command, " "))
				break
			}
			stdout, stderr, exitCode, rErr := gatedRunCommandBackend(ctx, canonicalWorkDir, step.Command)
			if rErr != nil {
				history += fmt.Sprintf("\nAction requested: run_command (command: %s)\nTool Result:\nError: %v\nNext action:", strings.Join(step.Command, " "), rErr)
				break
			}
			observation := fmt.Sprintf("exit code: %d\nstdout:\n%s\nstderr:\n%s", exitCode, stdout, stderr)
			if len(observation) > gatedGrepOutputCap {
				observation = observation[:gatedGrepOutputCap] + "\n...[truncated: run_command output exceeds 6000 bytes]..."
			}
			history += fmt.Sprintf("\nAction requested: run_command (command: %s)\nTool Result:\n%s\nNext action:", strings.Join(step.Command, " "), observation)
		case "grep":
			if strings.TrimSpace(step.Pattern) == "" {
				history += fmt.Sprintf("\nAction requested: grep\nTool Result:\nError: pattern must not be empty.\nNext action:")
				break
			}
			out, gErr := gatedGrepFiles(canonicalWorkDir, step.Path, step.Pattern)
			if gErr != nil {
				history += fmt.Sprintf("\nAction requested: grep (pattern: %s, path: %s)\nTool Result:\nError: %v\nNext action:", step.Pattern, step.Path, gErr)
			} else {
				history += fmt.Sprintf("\nAction requested: grep (pattern: %s, path: %s)\nTool Result:\n```\n%s\n```\nNext action:", step.Pattern, step.Path, out)
			}
		case "glob":
			if strings.TrimSpace(step.Pattern) == "" {
				history += fmt.Sprintf("\nAction requested: glob\nTool Result:\nError: pattern must not be empty.\nNext action:")
				break
			}
			matched, mErr := gatedGlobFiles(canonicalWorkDir, step.Path, step.Pattern)
			if mErr != nil {
				history += fmt.Sprintf("\nAction requested: glob (pattern: %s, path: %s)\nTool Result:\nError: %v\nNext action:", step.Pattern, step.Path, mErr)
			} else {
				history += fmt.Sprintf("\nAction requested: glob (pattern: %s, path: %s)\nTool Result:\n```\n%s\n```\nNext action:", step.Pattern, step.Path, matched)
			}
		default:
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\nUnknown action: %s. Use read_file, list_files, write_file, str_replace, run_command, grep, glob, or answer.\nNext action:", step.Action, step.Action)
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
