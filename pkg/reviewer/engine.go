package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/dedup"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

type Engine struct {
	cfg     *config.Config
	gh      *github.Client
	sandbox *sandbox.Runner
	llm     *llm.Client
}

func NewEngine(cfg *config.Config) *Engine {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Engine{
		cfg:     cfg,
		gh:      github.NewClient(cfg.GitHubToken),
		sandbox: sandbox.NewRunner(0),
		llm:     llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

func (e *Engine) ReviewPR(ctx context.Context, owner, repo string, number int) (*ReviewReport, error) {
	// 1. Fetch PR details
	pr, err := e.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch PR: %w", err)
	}

	// 2. Fetch Raw Diff with size limit
	diff, err := e.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch diff: %w", err)
	}

	// Limit diff size to prevent token overflow
	const maxDiffSize = 120000 // ~120KB, roughly 30k tokens
	if len(diff) > maxDiffSize {
		diff = diff[:maxDiffSize] + "\n\n... [diff truncated, exceeded size limit] ..."
	}

	// 3. Fetch Comments & Discussions
	generalComments, threads, err := e.gh.GetComments(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comments: %w", err)
	}

	// 4. Sandbox Verification (Optional/Configurable)
	var verificationSummary = "Sandbox verification skipped."
	var customRulesText = ""
	var rulesSource = ""
	sandboxVerified := false
	if e.cfg.EnableSandbox {
		workDir, cleanup, err := e.sandbox.PrepareWorkspace(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
		if err == nil {
			defer cleanup()
			verReport, err := e.sandbox.VerifyProject(ctx, workDir)
			if err == nil {
				verificationSummary = verReport.Summary
				customRulesText = verReport.CustomRules
				rulesSource = verReport.RulesSource
				sandboxVerified = len(verReport.Results) > 0
				// If tests or build failed, append stderr/stdout snippet
				for _, res := range verReport.Results {
					if !res.Passed {
						sandboxVerified = false
						verificationSummary += fmt.Sprintf("\nCommand `%s` failed (exit %d):\n```\n%s%s\n```",
							res.Command, res.ExitCode, res.Stdout, res.Stderr)
					}
				}
			}
		} else {
			verificationSummary = fmt.Sprintf("Sandbox checkout failed: %v", err)
		}
	}

	// 5. Build Prompts
	systemPrompt := buildSystemPrompt(customRulesText, rulesSource)
	userPrompt := buildUserPrompt(pr, diff, generalComments, threads, verificationSummary, customRulesText)

	// 6. Call LLM
	rawResponse, err := e.llm.ChatCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("llm call failed: %w", err)
	}

	// 7. Parse Structured JSON
	parsed, err := extractJSONOutput(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to parse review response: %w, raw: %s", err, rawResponse)
	}

	// 8. Deduplicate findings against previous comments on PR
	var allCommentBodies []string
	for _, c := range generalComments {
		allCommentBodies = append(allCommentBodies, c.Body)
	}
	for _, th := range threads {
		for _, tc := range th.Comments {
			allCommentBodies = append(allCommentBodies, tc.Body)
		}
	}
	existingFPs := dedup.ExtractFingerprints(allCommentBodies)

	var uniqueFindings []Finding
	skippedCount := 0
	for _, f := range parsed.Findings {
		fp := dedup.ComputeFingerprint(f.File, f.Line, f.Title, f.Severity)
		if existingFPs[fp] {
			skippedCount++
			continue
		}
		uniqueFindings = append(uniqueFindings, f)
	}

	report := &ReviewReport{
		PRNumber:            number,
		PRTitle:             pr.Title,
		HeadSHA:             pr.HeadSHA,
		Score:               parsed.Score,
		Summary:             parsed.Summary,
		VerificationSummary: verificationSummary,
		RulesSource:         rulesSource,
		DeduplicatedCount:   skippedCount,
		CommentFollowups:    parsed.CommentFollowups,
		Findings:            uniqueFindings,
	}
	if sandboxVerified {
		report.Suggestions = BuildInlineSuggestions(diff, uniqueFindings)
	}

	report.RawMarkdown = FormatReportMarkdown(report)
	return report, nil
}

func buildSystemPrompt(customRules, rulesSource string) string {
	base := `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining three critical signals:
1. Live Sandbox Verification (did the code compile, did tests pass in a real runner).
2. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
3. The Git Diff.

CRITICAL RULES:
- Never hallucinate false bugs. If a build or test already proved something works, do not claim otherwise.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. It must be compatible with verified sandbox results. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`

	if customRules != "" {
		base += fmt.Sprintf("\n\nCRITICAL: You MUST strictly enforce the project's repository custom instructions (from %s):\n```markdown\n%s\n```",
			rulesSource, customRules)
	}

	return base
}

func buildUserPrompt(pr *github.PRDetails, diff string, comments []github.Comment, threads []github.DiscussionThread, verification, customRules string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## PR Info\nTitle: %s\nAuthor: %s\nBase Branch: %s\nHead Branch: %s\n\n",
		pr.Title, pr.Author, pr.BaseRef, pr.HeadRef))

	if customRules != "" {
		b.WriteString("### Repository Review Instructions (Enforce Strictly):\n")
		b.WriteString(customRules + "\n\n")
	}

	if pr.Body != "" {
		b.WriteString(fmt.Sprintf("### PR Description:\n%s\n\n", pr.Body))
	}

	b.WriteString("### Real Sandbox Environment Verification:\n")
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

	b.WriteString("### Pull Request Git Diff:\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n\n")

	b.WriteString(`Respond with a JSON object with this exact shape:
{
  "score": 85,
  "summary": "High level overview of the PR quality and main areas of note.",
  "comment_followups": [
    {
      "thread_key": "path/to/file.go:42",
      "status": "ADDRESSED", // or "STILL_OPEN" / "DISMISSED"
      "note": "Author added nil check as requested by reviewer."
    }
  ],
  "findings": [
    {
      "file": "pkg/auth/token.go",
      "line": 54,
      "severity": "CRITICAL", // "CRITICAL", "WARNING", or "NOTE"
      "title": "Unbounded goroutine leak on context cancellation",
      "description": "The worker channel is never closed when ctx.Done() fires, leaving goroutines blocked.",
	  "suggestion": "Add a cancellation path before pushing into the channel.",
	  "suggested_code": "select {\\ncase ch <- value:\\ncase <-ctx.Done():\\n    return ctx.Err()\\n}"
    }
  ]
}`)

	return b.String()
}

func extractJSONOutput(raw string) (*LLMReviewOutput, error) {
	trimmed := strings.TrimSpace(raw)

	// Try to find JSON in the response (handle markdown codeblocks and extra text)
	jsonStart := strings.Index(trimmed, "{")
	jsonEnd := strings.LastIndex(trimmed, "}")

	if jsonStart >= 0 && jsonEnd > jsonStart {
		trimmed = trimmed[jsonStart : jsonEnd+1]
	}

	var output LLMReviewOutput
	if err := json.Unmarshal([]byte(trimmed), &output); err != nil {
		return nil, fmt.Errorf("JSON unmarshal failed: %w", err)
	}

	// Validate score range
	if output.Score < 0 {
		output.Score = 0
	} else if output.Score > 100 {
		output.Score = 100
	}

	// Validate findings
	for i := range output.Findings {
		if output.Findings[i].Severity != "CRITICAL" && output.Findings[i].Severity != "WARNING" && output.Findings[i].Severity != "NOTE" {
			output.Findings[i].Severity = "NOTE"
		}
		output.Findings[i].SuggestedCode = strings.TrimSpace(output.Findings[i].SuggestedCode)
	}

	// Validate comment followups
	for i := range output.CommentFollowups {
		if output.CommentFollowups[i].Status != "ADDRESSED" && output.CommentFollowups[i].Status != "STILL_OPEN" && output.CommentFollowups[i].Status != "DISMISSED" {
			output.CommentFollowups[i].Status = "STILL_OPEN"
		}
	}

	return &output, nil
}

// BuildInlineSuggestions returns only safe GitHub suggestion blocks. GitHub accepts
// suggestions only on added PR lines, so every target is verified against raw diff.
func BuildInlineSuggestions(diff string, findings []Finding) []github.InlineSuggestion {
	changedLines := changedPRLines(diff)
	suggestions := make([]github.InlineSuggestion, 0, len(findings))
	for _, finding := range findings {
		code := strings.TrimSpace(finding.SuggestedCode)
		if code == "" || strings.Contains(code, "```") || !changedLines[finding.File][finding.Line] {
			continue
		}
		body := fmt.Sprintf("**[%s] %s**\n\n%s\n\n```suggestion\n%s\n```", finding.Severity, finding.Title, finding.Description, code)
		suggestions = append(suggestions, github.InlineSuggestion{Path: finding.File, Line: finding.Line, Body: body})
	}
	return suggestions
}

func changedPRLines(diff string) map[string]map[int]bool {
	lines := make(map[string]map[int]bool)
	var path string
	var line int
	inHunk := false
	for _, raw := range strings.Split(diff, "\n") {
		if strings.HasPrefix(raw, "+++ b/") {
			path = strings.TrimPrefix(raw, "+++ b/")
			inHunk = false
			continue
		}
		if strings.HasPrefix(raw, "@@") {
			fields := strings.Fields(raw)
			if len(fields) >= 3 {
				newRange := strings.TrimPrefix(fields[2], "+")
				startText, _, _ := strings.Cut(newRange, ",")
				start, err := strconv.Atoi(startText)
				if err == nil {
					line, inHunk = start, true
				}
			}
			continue
		}
		if !inHunk || path == "" || raw == "" || strings.HasPrefix(raw, "\\") {
			continue
		}
		switch raw[0] {
		case '+':
			if lines[path] == nil {
				lines[path] = make(map[int]bool)
			}
			lines[path][line] = true
			line++
		case '-':
			// Removed line does not exist on RIGHT side of review.
		default:
			line++
		}
	}
	return lines
}

func FormatReportMarkdown(r *ReviewReport) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## 🤖 PR Review Report: %s (Score: %d/100)\n\n", r.PRTitle, r.Score))
	if r.RulesSource != "" {
		sb.WriteString(fmt.Sprintf("📋 **Custom Guidelines Enforced:** `%s`\n\n", r.RulesSource))
	}
	sb.WriteString(fmt.Sprintf("**Sandbox Verification:** %s\n\n", r.VerificationSummary))
	sb.WriteString(fmt.Sprintf("### Summary\n%s\n\n", r.Summary))

	if len(r.CommentFollowups) > 0 {
		sb.WriteString("### 💬 Discussion & Feedback Follow-ups\n")
		for _, cf := range r.CommentFollowups {
			icon := "ℹ️"
			if cf.Status == "ADDRESSED" {
				icon = "✅"
			} else if cf.Status == "STILL_OPEN" {
				icon = "⚠️"
			}
			sb.WriteString(fmt.Sprintf("- %s **[%s]** `%s`: %s\n", icon, cf.Status, cf.ThreadKey, cf.Note))
		}
		sb.WriteString("\n")
	}

	if len(r.Findings) == 0 {
		if r.DeduplicatedCount > 0 {
			sb.WriteString(fmt.Sprintf("### 🎯 Findings\nAll %d detected issues were already reported previously and have been deduplicated.\n\n", r.DeduplicatedCount))
		} else {
			sb.WriteString("### 🎯 Findings\nNo critical bugs or defects detected. Looks ready to merge!\n\n")
		}
	} else {
		sb.WriteString("### 🎯 Findings\n")
		if r.DeduplicatedCount > 0 {
			sb.WriteString(fmt.Sprintf("ℹ️ *%d duplicate findings previously reported were filtered out.*\n\n", r.DeduplicatedCount))
		}
		for _, f := range r.Findings {
			sevIcon := "⚠️"
			if f.Severity == "CRITICAL" {
				sevIcon = "🚨"
			} else if f.Severity == "NOTE" {
				sevIcon = "💡"
			}

			fp := dedup.ComputeFingerprint(f.File, f.Line, f.Title, f.Severity)
			sb.WriteString(fmt.Sprintf("#### %s [%s] %s (`%s:%d`)\n", sevIcon, f.Severity, f.Title, f.File, f.Line))
			sb.WriteString(fmt.Sprintf("<!-- pr-review-go:fingerprint=%s -->\n", fp))
			sb.WriteString(fmt.Sprintf("%s\n\n", f.Description))
			if f.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("> **Suggestion:** %s\n\n", f.Suggestion))
			}
		}
	}

	return sb.String()
}
