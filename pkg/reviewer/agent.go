package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

var (
	// ErrModelViolationsExceeded indicates the model requested too many forbidden mutation actions.
	ErrModelViolationsExceeded = errors.New("model exceeded mutation violation limit")
)

// AgentAction represents a single JSON tool or answer request from the model.
type AgentAction struct {
	Action    string           `json:"action"`
	Path      string           `json:"path,omitempty"`
	Query     string           `json:"query,omitempty"`
	Pattern   string           `json:"pattern,omitempty"`
	Score     *int             `json:"score,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	Findings  []Finding        `json:"findings,omitempty"`
	FinalText string           `json:"final_text,omitempty"`
	Review    *LLMReviewOutput `json:"review,omitempty"`
}

// AgentReviewResult holds the outcome of the agentic exploration loop.
type AgentReviewResult struct {
	Output          *LLMReviewOutput
	Ledger          *diff.CoverageLedger
	Inventory       *diff.ChangeInventory
	TurnsUsed       int
	BytesConsumed   int64
	Violations      int
	BudgetExhausted bool
	FilesServed     []string
}

// ReviewerAgent runs a bounded read-only agent loop over a repository snapshot.
type ReviewerAgent struct {
	cfg *config.Config
	llm *llm.Client
}

// NewReviewerAgent creates a new agent with the given configuration and LLM client.
func NewReviewerAgent(cfg *config.Config, llmClient *llm.Client) *ReviewerAgent {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &ReviewerAgent{
		cfg: cfg,
		llm: llmClient,
	}
}

// Run executes the bounded exploration loop over the change inventory and snapshot.
func (a *ReviewerAgent) Run(
	ctx context.Context,
	pr *github.PRDetails,
	inv *diff.ChangeInventory,
	ledger *diff.CoverageLedger,
	snap *sandbox.Snapshot,
	verificationSummary string,
	verificationStatus sandbox.VerificationStatus,
	comments []github.Comment,
	threads []github.DiscussionThread,
) (*AgentReviewResult, error) {
	// Validate snapshot before first tool use
	if snap != nil {
		if err := snap.Validate(); err != nil {
			return nil, fmt.Errorf("snapshot validation failed before agent exploration: %w", err)
		}
	}

	maxTurns := config.DefaultAgentMaxTurns
	violationLimit := config.DefaultAgentModelViolationLimit
	if a.cfg != nil {
		if a.cfg.AgentMaxTurns > 0 {
			maxTurns = a.cfg.AgentMaxTurns
		}
		if a.cfg.AgentModelViolationLimit > 0 {
			violationLimit = a.cfg.AgentModelViolationLimit
		}
	}

	var tools *SnapshotTools
	if snap != nil && snap.SourceDir != "" {
		st, err := NewSnapshotTools(snap.SourceDir, a.cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize snapshot tools: %w", err)
		}
		tools = st
	}

	systemPrompt := a.buildAgentSystemPrompt(verificationStatus, maxTurns)
	history := a.buildAgentInitialUserPrompt(pr, inv, comments, threads, verificationSummary, verificationStatus)

	filesServedMap := make(map[string]bool)
	var filesServed []string
	violations := 0
	turnsUsed := 0
	var finalOutput *LLMReviewOutput
	budgetExhausted := false

	for turn := 0; turn < maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		turnsUsed++
		rawResponse, err := a.llm.ChatCompletion(ctx, systemPrompt, history)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("llm call failed: %w", err)
		}

		action, parseErr := parseAgentAction(rawResponse)
		if parseErr != nil {
			// Plain text or unparseable: check if extractJSONOutput can parse a final review
			if out, err := extractJSONOutput(rawResponse); err == nil {
				finalOutput = out
				break
			}
			history += fmt.Sprintf("\nAssistant: %s\nUser: Please reply with a single JSON action object (e.g. read_file, list_files, search_files, or answer).", rawResponse)
			continue
		}

		// Check if action is answer
		if action.Action == "answer" || (action.Action == "" && (action.Score != nil || len(action.Findings) > 0 || action.Summary != "")) {
			if action.Review != nil {
				finalOutput = action.Review
			} else if action.Score != nil || len(action.Findings) > 0 || action.Summary != "" {
				score := 70
				if action.Score != nil {
					score = *action.Score
				}
				finalOutput = &LLMReviewOutput{
					Score:    score,
					Summary:  action.Summary,
					Findings: action.Findings,
				}
			} else if action.FinalText != "" {
				if out, err := extractJSONOutput(action.FinalText); err == nil {
					finalOutput = out
				} else {
					finalOutput = &LLMReviewOutput{
						Score:   75,
						Summary: action.FinalText,
					}
				}
			} else {
				// Fallback to extractJSONOutput on raw response
				if out, err := extractJSONOutput(rawResponse); err == nil {
					finalOutput = out
				}
			}
			break
		}

		// Handle mutation violations
		if isMutationAction(action.Action) {
			violations++
			refusalMsg := fmt.Sprintf("Error: %s is disabled: the reviewer is strictly read-only and mutations/command execution are not permitted.", action.Action)
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action (or provide final answer via action='answer'):", action.Action, refusalMsg)
			if violations >= violationLimit {
				return nil, fmt.Errorf("%w: refused %d mutation attempts (%s)", ErrModelViolationsExceeded, violations, action.Action)
			}
			continue
		}

		// Execute read-only tools
		if tools == nil {
			history += "\nTool Result:\nSnapshot is not available for file reads.\nNext action:"
			continue
		}

		var toolResult string
		switch action.Action {
		case "read_file":
			content, rErr := tools.ReadFile(action.Path)
			if rErr != nil {
				toolResult = fmt.Sprintf("Error reading file: %v", rErr)
			} else {
				toolResult = content
				norm := normalizeFilePath(action.Path)
				if !filesServedMap[norm] {
					filesServedMap[norm] = true
					filesServed = append(filesServed, norm)
				}
				// Mark file examined in ledger
				if ledger != nil {
					ledger.MarkFileExamined(norm)
					// Also check if inventory path matches without prefix
					if inv != nil {
						if f := inv.FindFile(norm); f != nil {
							ledger.MarkFileExamined(f.Path)
						}
					}
				}
			}
			history += fmt.Sprintf("\nAction requested: read_file (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", action.Path, toolResult)

		case "list_files":
			p := action.Path
			if p == "" {
				p = "."
			}
			listing, lErr := tools.ListFiles(p)
			if lErr != nil {
				toolResult = fmt.Sprintf("Error listing files: %v", lErr)
			} else {
				toolResult = listing
			}
			history += fmt.Sprintf("\nAction requested: list_files (path: %s)\nTool Result:\n```\n%s\n```\nNext action:", p, toolResult)

		case "search_files":
			q := action.Query
			if q == "" {
				q = action.Pattern
			}
			matches, sErr := tools.SearchFiles(q)
			if sErr != nil {
				toolResult = fmt.Sprintf("Error searching files: %v", sErr)
			} else {
				toolResult = matches
			}
			history += fmt.Sprintf("\nAction requested: search_files (query: %s)\nTool Result:\n```\n%s\n```\nNext action:", q, toolResult)

		default:
			toolResult = fmt.Sprintf("Unknown action: %s. Use read_file, list_files, search_files, or answer.", action.Action)
			history += fmt.Sprintf("\nAction requested: %s\nTool Result:\n%s\nNext action:", action.Action, toolResult)
		}

		if tools.IsBudgetExhausted() {
			budgetExhausted = true
			break
		}
	}

	if turnsUsed >= maxTurns {
		budgetExhausted = true
	}

	// If no final output was generated during turns (budget reached before answer action)
	if finalOutput == nil {
		budgetExhausted = true
		// One concluding attempt
		finalPrompt := history + "\nBudget exhausted. Based on the files examined so far, provide your final review now conforming to the review schema via action='answer'."
		conclResp, err := a.llm.ChatCompletion(ctx, systemPrompt, finalPrompt)
		if err == nil {
			if out, parseErr := extractJSONOutput(conclResp); parseErr == nil {
				finalOutput = out
			} else if act, aErr := parseAgentAction(conclResp); aErr == nil && act.Action == "answer" {
				if act.Review != nil {
					finalOutput = act.Review
				} else {
					score := 70
					if act.Score != nil {
						score = *act.Score
					}
					finalOutput = &LLMReviewOutput{
						Score:    score,
						Summary:  act.Summary,
						Findings: act.Findings,
					}
				}
			}
		}

		// If still nil, fail with structured parse error
		if finalOutput == nil {
			return nil, fmt.Errorf("failed to parse review response: model output could not be parsed into valid review JSON")
		}
	}

	var bytesConsumed int64
	if tools != nil {
		bytesConsumed = tools.BytesConsumed()
		if tools.IsBudgetExhausted() {
			budgetExhausted = true
		}
	}

	return &AgentReviewResult{
		Output:          finalOutput,
		Ledger:          ledger,
		Inventory:       inv,
		TurnsUsed:       turnsUsed,
		BytesConsumed:   bytesConsumed,
		Violations:      violations,
		BudgetExhausted: budgetExhausted,
		FilesServed:     filesServed,
	}, nil
}

func (a *ReviewerAgent) buildAgentSystemPrompt(status any, maxTurns int) string {
	basePrompt := buildSystemPrompt(status)
	return basePrompt + fmt.Sprintf(`

### AGENTIC REVIEW TOOL PROTOCOL:
You are an autonomous code reviewer inspecting a verified immutable PR snapshot.
Instead of receiving a blindly truncated diff, you have access to read-only snapshot inspection tools:

AVAILABLE ACTIONS:
1. "read_file": Read content of a file in the PR snapshot.
   Format: {"action": "read_file", "path": "path/to/file"}
2. "list_files": List files in a directory in the snapshot.
   Format: {"action": "list_files", "path": "."}
3. "search_files": Search for a text pattern in files across the snapshot.
   Format: {"action": "search_files", "query": "text"}
4. "answer": Conclude your review with score, summary, and findings.
   Format: {"action": "answer", "score": 85, "summary": "...", "findings": [...]}

RULES & RESTRICTIONS:
- You have up to %d iterative tool turns to inspect changed files, related tests, and callers.
- The review is strictly read-only. Modifying files ("write_file"), running shell commands ("run_command"), or git push ("commit_and_push") are strictly forbidden and will be refused.
- Output MUST be a single strict JSON object matching {"action": "...", ...}.
- Do not wrap in markdown code blocks.`, maxTurns)
}

func (a *ReviewerAgent) buildAgentInitialUserPrompt(
	pr *github.PRDetails,
	inv *diff.ChangeInventory,
	comments []github.Comment,
	threads []github.DiscussionThread,
	verification string,
	status any,
) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## PR Info\nTitle: %s\nAuthor: %s\nBase Branch: %s\nHead Branch: %s\n\n",
		pr.Title, pr.Author, pr.BaseRef, pr.HeadRef))

	if pr.Body != "" {
		b.WriteString(fmt.Sprintf("### PR Description:\n%s\n\n", pr.Body))
	}

	st := parseVerificationStatus(status)
	switch st {
	case sandbox.StatusPassed:
		b.WriteString("### Real Sandbox Environment Verification:\n")
	case sandbox.StatusBuildFailed, sandbox.StatusTestFailed:
		b.WriteString("### Real Sandbox Environment Verification (Failed):\n")
	default:
		b.WriteString("### Sandbox Verification (Skipped / Unavailable):\n")
	}
	b.WriteString(verification + "\n\n")

	b.WriteString("### Existing PR Discussion & Review Threads:\n")
	if len(comments) == 0 && len(threads) == 0 {
		b.WriteString("No prior comments or review threads.\n\n")
	} else {
		for _, c := range comments {
			b.WriteString(fmt.Sprintf("- [@%s]: %s\n", c.User, c.Body))
		}
		for _, th := range threads {
			b.WriteString(fmt.Sprintf("\n#### Thread on %s (Line %d):\n", th.Path, th.Line))
			for _, tc := range th.Comments {
				b.WriteString(fmt.Sprintf("  - [@%s]: %s\n", tc.User, tc.Body))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("### Change Map (Modified Files & Hunks):\n")
	if inv != nil && len(inv.Files) > 0 {
		for _, f := range inv.Files {
			statusStr := "modified"
			if f.IsNew {
				statusStr = "added"
			} else if f.IsDeleted {
				statusStr = "deleted"
			} else if f.IsRename {
				statusStr = fmt.Sprintf("renamed from %s", f.OldPath)
			}
			b.WriteString(fmt.Sprintf("- `%s` (%s, %d hunks)\n", f.Path, statusStr, len(f.Hunks)))
			for _, h := range f.Hunks {
				b.WriteString(fmt.Sprintf("  - Hunk %s: %s (lines +%d..+%d)\n", h.ID, h.Header, h.NewStart, h.NewStart+h.NewCount-1))
			}
		}
	} else {
		b.WriteString("No files modified.\n")
	}
	b.WriteString("\n")

	b.WriteString("Begin exploring by reading the modified files with action='read_file', or deliver your review via action='answer'.\n")
	return b.String()
}

func isMutationAction(act string) bool {
	lower := strings.ToLower(strings.TrimSpace(act))
	switch lower {
	case "write_file", "run_command", "commit_and_push", "exec", "bash", "shell", "push", "commit", "delete_file":
		return true
	}
	if strings.Contains(lower, "write") || strings.Contains(lower, "command") ||
		strings.Contains(lower, "commit") || strings.Contains(lower, "push") ||
		strings.Contains(lower, "exec") {
		return true
	}
	return false
}

func parseAgentAction(raw string) (*AgentAction, error) {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	} else {
		return nil, fmt.Errorf("no json object found")
	}

	var action AgentAction
	if err := json.Unmarshal([]byte(trimmed), &action); err != nil {
		return nil, err
	}
	return &action, nil
}

func normalizeFilePath(p string) string {
	clean := filepath.Clean(p)
	slash := filepath.ToSlash(clean)
	return strings.TrimPrefix(slash, "./")
}
