package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

var (
	// ErrModelViolationsExceeded indicates the model requested too many forbidden mutation actions.
	ErrModelViolationsExceeded = errors.New("model exceeded mutation violation limit")

	// ErrAmbiguousAgentReply marks a reply that mixes wire formats or mixes an
	// answer with tool calls. Such a reply is rejected whole and nudged, never
	// partially executed, so no action or finding is silently dropped.
	ErrAmbiguousAgentReply = errors.New("ambiguous agent reply")
)

// logVerboseTurns reports whether full per-turn replies should be logged.
func logVerboseTurns() bool {
	return strings.TrimSpace(os.Getenv("REVIEWER_VERBOSE_TURNS")) == "1"
}

// maxEarlyAnswerPushes bounds how many times a model may try to conclude before
// it has read any modified file. Live PR #2 needed more than one nudge: the model
// answered on turn 1, was pushed back, and answered again without reading, so a
// single pushback let it burn the whole turn budget on nothing.
const maxEarlyAnswerPushes = 3

// maxActionsPerReply bounds how many tool calls from one reply are executed, in
// order. DSML-emitting models batch several reads per reply; this keeps a single
// reply from doing unbounded work while still using what the model asked for.
const maxActionsPerReply = 5

// maxEmptyResponseRetries bounds how many times a single turn is re-requested
// after the provider returned no usable completion at all. The LLM client has
// already tried the other models in LLM_FALLBACK_MODELS by this point, so one
// extra attempt is enough; more just burns the job's wall-clock budget.
const maxEmptyResponseRetries = 1

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
	readAttempts := make(map[string]bool)
	var filesServed []string
	violations := 0
	turnsUsed := 0
	var finalOutput *LLMReviewOutput
	budgetExhausted := false

	// REV-READ-01: an answer that lands before any modified file has been served
	// is rejected once, so the model cannot report on code it never read.
	hasExaminableChanges := tools != nil && ledger != nil && ledger.ExaminableCount() > 0
	earlyAnswerPushes := 0
	// An empty completion is a provider fault; retry the same turn a couple of
	// times before failing the job, so a flaky backend does not discard a review
	// that was already half-explored.
	emptyResponseRetries := 0
	earlyAnswerPushback := func() {
		earlyAnswerPushes++
		example := firstExaminableFile(inv)
		history += fmt.Sprintf("\nAction rejected: cannot conclude review via action='answer' without reading any modified files. You must inspect the modified files using action='read_file' first (e.g. read %s). Do not fabricate findings without examining code.\nNext action:", example)
	}

	for turn := 0; turn < maxTurns; turn++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		turnsUsed++
		turnStarted := time.Now()
		log.Printf("[reviewer] turn %d/%d start (history %d bytes, %d files served)", turnsUsed, maxTurns, len(history), len(filesServed))
		rawResponse, err := a.llm.ChatCompletion(ctx, systemPrompt, history)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A provider that returns an empty completion is a transient provider
			// fault, not a review verdict. The LLM client already retried with a
			// doubled budget, so retry here at the loop level without spending a
			// turn: the model never got to act, so nothing was learned or lost.
			if errors.Is(err, llm.ErrEmptyAssistantContent) && emptyResponseRetries < maxEmptyResponseRetries {
				emptyResponseRetries++
				log.Printf("[reviewer] empty completion on turn %d (attempt %d/%d, history %d bytes); retrying the same turn",
					turnsUsed, emptyResponseRetries, maxEmptyResponseRetries, len(history))
				turnsUsed--
				continue
			}
			// Report where the loop was when the provider gave up: turn number,
			// history size and how many files had been served make a bare
			// "empty assistant response content" diagnosable from the job row.
			return nil, fmt.Errorf("llm call failed (turn %d/%d, history %d bytes, %d files served): %w",
				turnsUsed, maxTurns, len(history), len(filesServed), err)
		}

		log.Printf("[reviewer] turn %d/%d done in %s (%d response bytes)", turnsUsed, maxTurns, time.Since(turnStarted).Round(time.Second), len(rawResponse))
		if logVerboseTurns() {
			log.Printf("[reviewer] turn %d/%d reply: %.200s", turnsUsed, maxTurns, strings.ReplaceAll(rawResponse, "\n", " "))
		}

		actions, parseErr := parseAgentActions(rawResponse)
		if parseErr != nil {
			// A reply that mixes protocols or mixes answer with tool calls is
			// rejected whole; extracting the JSON half would silently drop the
			// tool calls, so nudge instead of falling through to the JSON path.
			if errors.Is(parseErr, ErrAmbiguousAgentReply) {
				history += fmt.Sprintf("\nAssistant: %s\nAction rejected: %v. Reply with exactly one protocol, and either a single action='answer' or up to %d read-only tool calls.\nNext action:", rawResponse, parseErr, maxActionsPerReply)
				continue
			}
			if out, err := extractJSONOutput(rawResponse); err == nil {
				actions = []*AgentAction{{Action: "answer", Review: out}}
			} else {
				history += fmt.Sprintf("\nAssistant: %s\nUser: Please reply with a single JSON action object (e.g. read_file, list_files, search_files, or answer).", rawResponse)
				continue
			}
		}

		if isAnswerAction(actions[0]) && tools != nil && !tools.IsBudgetExhausted() && turnsUsed < maxTurns {
			if pending := remainingReadActions(inv, readAttempts); len(pending) > 0 {
				history += "\nAction rejected: changed files remain unread. The following action='read_file' calls provide missing evidence. Reconsider your findings using this content and investigate relevant definitions, callers and tests before answering again. Also weigh each newly served file against the other applicable categories (security and input validation, concurrency, logic) so a severe defect in one category does not end the review.\n"
				log.Printf("[reviewer] coverage completion: serving %d unread changed files before accepting answer", len(pending))
				actions = pending
			}
		}

		// An answer is never mixed with tool calls (that reply is rejected above).
		action := actions[0]
		if isAnswerAction(action) {
			if hasExaminableChanges && len(filesServed) == 0 && earlyAnswerPushes < maxEarlyAnswerPushes {
				earlyAnswerPushback()
				continue
			}
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

		// Execute the reply's tool calls in order, bounded per reply; the same
		// turn, violation and byte-budget limits apply to every action.
		skipped := 0
		if len(actions) > maxActionsPerReply {
			skipped = len(actions) - maxActionsPerReply
			actions = actions[:maxActionsPerReply]
		}
		for i, action := range actions {
			if err := ctx.Err(); err != nil {
				return nil, err
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
				readAttempts[normalizeFilePath(action.Path)] = true
				content, complete, rErr := tools.readFile(action.Path)
				if rErr != nil {
					toolResult = fmt.Sprintf("Error reading file: %v", rErr)
				} else {
					toolResult = content
					norm := normalizeFilePath(action.Path)
					if !filesServedMap[norm] {
						filesServedMap[norm] = true
						filesServed = append(filesServed, norm)
					}
					// Truncated content is useful context, but not full-file coverage.
					if ledger != nil && complete {
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
				skipped += len(actions) - i - 1
				break
			}
		}
		if skipped > 0 {
			history += fmt.Sprintf("\nNote: %d further action(s) from that reply were not executed (limit %d tool calls per reply, or the tool byte budget is exhausted).\nNext action:", skipped, maxActionsPerReply)
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
4. "answer": Conclude your review with score, summary, and findings. Only deliver your answer after reading the modified files with action='read_file'.
   Format: {"action": "answer", "score": 85, "summary": "...", "findings": [...]}
   Each finding uses this shape: {"file":"path/to/file.go","line":42,"severity":"CRITICAL","title":"Concrete defect","description":"Evidence, triggering conditions, and impact","suggestion":"A safe correction"}.
   Use the exact repository-relative file path and source line supported by the inspected code. Severity is CRITICAL, WARNING, or NOTE. Do not invent locations or unseen behavior. Findings may be an empty array.

RULES & RESTRICTIONS:
- You have up to %d iterative tool turns to inspect changed files, related tests, and callers.
- Start with the modified files, then use targeted search and reads to verify suspected bugs against definitions, callers, tests, or configuration. Do not guess unseen behavior or crawl unrelated files.
- Check the modified files against the applicable concern categories (security and input validation, concurrency, logic and correctness). Finding one severe defect is not a reason to stop early or to cap the number of findings.
- The review is strictly read-only. Modifying files ("write_file"), running shell commands ("run_command"), or git push ("commit_and_push") are strictly forbidden and will be refused.
- Output one strict JSON action object, or up to %d consecutive JSON objects for independent read-only tool calls. An answer MUST be a single object and never mixed with tool calls.
- Do not wrap in markdown code blocks.`, maxTurns, maxActionsPerReply)
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
	case sandbox.StatusConfinementDenied:
		b.WriteString("### Real Sandbox Environment Verification (Confinement Denied):\n")
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

	// The change map above is deliberately metadata only: file paths and hunk
	// headers, never code. State that plainly, because a model shown a short
	// change map otherwise concludes it has enough to review and answers on turn
	// one (observed live on thozoz/aura-web PR #2: twelve turns, zero files
	// read, so the zero-coverage gate correctly refused to publish anything).
	b.WriteString("IMPORTANT: this change map contains NO source code — only file paths and hunk headers.\n")
	b.WriteString("You cannot review anything from it. Your FIRST action must be action='read_file' on a modified file.\n")
	b.WriteString("Read every modified non-test source file before you conclude, weighing each against the applicable concern categories (security and input validation, concurrency, logic and correctness), then answer with action='answer'.\n")
	return b.String()
}

// firstExaminableFile returns the path of the first file in the inventory that
// holds real code hunks (non-binary, non-deleted, non-mode-only), or "" if none.
func firstExaminableFile(inv *diff.ChangeInventory) string {
	if inv == nil {
		return ""
	}
	for _, f := range inv.Files {
		if f == nil || f.IsBinary || f.IsDeleted || f.IsModeOnly || len(f.Hunks) == 0 {
			continue
		}
		return f.Path
	}
	return ""
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

// parseAgentAction decodes the model's requested action from one reply.
//
// Two wire shapes are accepted, because both are observed in production:
//
//  1. JSON, possibly several concatenated objects in one reply:
//     {"action": "read_file", "path": "a.go"}
//     {"action": "read_file", "path": "b.go"}
//     Slicing from the first '{' to the LAST '}' turns that into invalid JSON, so
//     a stream decoder is used to accept the first complete object.
//
//  2. XML tool-call markup, emitted by models that were trained on a function-
//     calling dialect rather than this protocol:
//     <tool_calls><invoke name="read_file">
//     <parameter name="path">a.go</parameter>
//     </invoke></tool_calls>
//     This shape cost every turn of a live review (thozoz/aura-web PR #2): the
//     JSON decoder rejected it, no file was ever read, and the zero-coverage gate
//     correctly refused to publish the result.
func parseAgentAction(raw string) (*AgentAction, error) {
	actions, err := parseAgentActions(raw)
	if err != nil {
		return nil, err
	}
	return actions[0], nil
}

// parseAgentActions decodes every action in one reply, in order.
//
// Two wire shapes are accepted, because both are observed in production:
//
//  1. JSON, possibly several concatenated objects. A stream decoder collects
//     each complete object.
//  2. XML function-call markup, plain or with the DeepSeek DSML prefix, e.g.
//     an invoke element named read_file holding a parameter element for path.
//     Live thozoz/aura-web PR #3 spent 8 of 12 turns emitting DSML: the JSON
//     decoder rejected it and no file was ever read.
//
// A reply mixing JSON with XML, or an answer with tool calls, is ambiguous: it
// is rejected whole rather than silently dropping half of it.
func parseAgentActions(raw string) ([]*AgentAction, error) {
	xmlActions := parseXMLAgentActions(raw)
	jsonActions := parseJSONAgentActions(xmlInvokePattern.ReplaceAllString(raw, ""))
	if len(xmlActions) > 0 && len(jsonActions) > 0 {
		return nil, fmt.Errorf("%w: reply mixes JSON and XML tool calls", ErrAmbiguousAgentReply)
	}
	actions := xmlActions
	if len(actions) == 0 {
		actions = jsonActions
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("no agent action found")
	}

	var answers, calls int
	for _, a := range actions {
		if isAnswerAction(a) {
			answers++
		} else {
			calls++
		}
	}
	if answers > 0 && calls > 0 {
		return nil, fmt.Errorf("%w: reply mixes action='answer' with tool calls", ErrAmbiguousAgentReply)
	}
	if answers > 1 {
		return nil, fmt.Errorf("%w: reply contains %d answers", ErrAmbiguousAgentReply, answers)
	}
	return actions, nil
}

// isAnswerAction reports whether an action concludes the review.
func isAnswerAction(action *AgentAction) bool {
	return action.Action == "answer" ||
		(action.Action == "" && (action.Score != nil || len(action.Findings) > 0 || action.Summary != ""))
}

// parseJSONAgentActions decodes concatenated JSON action objects in order.
func parseJSONAgentActions(raw string) []*AgentAction {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed[start:]))
	var actions []*AgentAction
	for {
		var action AgentAction
		if err := dec.Decode(&action); err != nil {
			break
		}
		actions = append(actions, &action)
	}
	return actions
}

// dsmlPrefix matches the optional DeepSeek DSML marker before a tag name, as in
// the fullwidth-pipe form. Plain tags keep matching with the group absent.
const dsmlPrefix = `(?:[｜|]DSML[｜|]\s+)?`

// xmlInvokePattern captures an invoke block and its raw attribute list.
var xmlInvokePattern = regexp.MustCompile(`(?is)<` + dsmlPrefix + `invoke\b([^>]*)>(.*?)</` + dsmlPrefix + `invoke\s*>`)

// xmlParameterPattern captures a parameter block and its raw attribute list.
var xmlParameterPattern = regexp.MustCompile(`(?is)<` + dsmlPrefix + `parameter\b([^>]*)>(.*?)</` + dsmlPrefix + `parameter\s*>`)

// xmlAttrPattern captures key="value" or key='value' attributes in any order.
var xmlAttrPattern = regexp.MustCompile(`(?is)([a-z_][a-z0-9_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// xmlAttrValue returns the value of attr from a raw attribute list.
func xmlAttrValue(attrs, attr string) string {
	for _, m := range xmlAttrPattern.FindAllStringSubmatch(attrs, -1) {
		if !strings.EqualFold(m[1], attr) {
			continue
		}
		if m[2] != "" {
			return m[2]
		}
		return m[3]
	}
	return ""
}

// xmlToolAliases maps the function names used by other dialects onto this
// protocol's action names.
var xmlToolAliases = map[string]string{
	"read_file":    "read_file",
	"read":         "read_file",
	"cat":          "read_file",
	"view_file":    "read_file",
	"list_files":   "list_files",
	"list":         "list_files",
	"ls":           "list_files",
	"search_files": "search_files",
	"search":       "search_files",
	"grep":         "search_files",
	"answer":       "answer",
	"submit":       "answer",
}

// parseXMLAgentActions extracts every usable tool call from XML markup, in
// order. Unknown tools and calls with no usable argument are skipped so a later
// well-formed call in the same reply still runs.
func parseXMLAgentActions(raw string) []*AgentAction {
	var actions []*AgentAction
	for _, match := range xmlInvokePattern.FindAllStringSubmatch(raw, -1) {
		tool := strings.ToLower(strings.TrimSpace(xmlAttrValue(match[1], "name")))
		actionName, ok := xmlToolAliases[tool]
		if !ok {
			continue
		}

		params := make(map[string]string)
		for _, pm := range xmlParameterPattern.FindAllStringSubmatch(match[2], -1) {
			key := strings.ToLower(strings.TrimSpace(xmlAttrValue(pm[1], "name")))
			if key == "" {
				continue
			}
			params[key] = strings.TrimSpace(pm[2])
		}

		action := &AgentAction{Action: actionName}
		switch actionName {
		case "read_file":
			action.Path = firstNonEmpty(params["path"], params["file"], params["file_path"], params["filename"])
		case "list_files":
			action.Path = firstNonEmpty(params["path"], params["dir"], params["directory"])
		case "search_files":
			action.Query = firstNonEmpty(params["query"], params["pattern"], params["text"], params["term"])
			action.Pattern = action.Query
		case "answer":
			// XML/DSML answers carry the same JSON findings array as JSON
			// answers. Never turn supplied evidence into an empty review.
			if findings, ok := params["findings"]; ok {
				if err := json.Unmarshal([]byte(findings), &action.Findings); err != nil {
					continue
				}
			}
			action.Summary = params["summary"]
			if score := params["score"]; score != "" {
				if n, err := strconv.Atoi(score); err == nil {
					action.Score = &n
				}
			}
			if len(action.Summary) == 0 {
				continue
			}
		}

		// A tool call whose argument is missing would be rejected by the tool
		// anyway, but skipping it here lets the loop try the next block in the
		// same reply instead of spending a whole turn on a malformed call.
		if actionName != "answer" && action.Path == "" && action.Query == "" {
			continue
		}
		actions = append(actions, action)
	}
	return actions
}

// firstNonEmpty returns the first non-blank candidate.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// remainingReadActions completes evidence delivery without retrying failed reads.
func remainingReadActions(inv *diff.ChangeInventory, attempted map[string]bool) []*AgentAction {
	if inv == nil {
		return nil
	}
	var actions []*AgentAction
	seen := make(map[string]bool)
	for _, file := range inv.Files {
		if file.IsBinary || file.IsDeleted || file.IsModeOnly || len(file.Hunks) == 0 {
			continue
		}
		path := normalizeFilePath(file.Path)
		if attempted[path] || seen[path] {
			continue
		}
		seen[path] = true
		actions = append(actions, &AgentAction{Action: "read_file", Path: path})
		if len(actions) == maxActionsPerReply {
			break
		}
	}
	return actions
}

func normalizeFilePath(p string) string {
	clean := filepath.Clean(p)
	slash := filepath.ToSlash(clean)
	return strings.TrimPrefix(slash, "./")
}
