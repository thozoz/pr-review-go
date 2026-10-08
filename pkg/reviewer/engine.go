package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/dedup"
	"github.com/thozoz/pr-review-go/pkg/diff"
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
	gh := github.NewClientFromConfig(cfg)
	return &Engine{
		cfg:     cfg,
		gh:      gh,
		sandbox: sandbox.NewPlatformRunner(cfg, gh, nil, nil),
		llm:     llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// NewEngineWithClients creates an Engine with injected dependencies for isolated tests.
func NewEngineWithClients(cfg *config.Config, gh *github.Client, llmClient *llm.Client, sandboxRunner *sandbox.Runner) *Engine {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &Engine{
		cfg:     cfg,
		gh:      gh,
		sandbox: sandboxRunner,
		llm:     llmClient,
	}
}

func (e *Engine) SetSandbox(s *sandbox.Runner) {
	e.sandbox = s
}

// ReviewPRAtHead verifies the expected commit OID, fetches PR details, ensures
// the PR head matches expectedHead, retrieves the diff using GetDiffAtCommits,
// and executes the review bound to that immutable head.
func (e *Engine) ReviewPRAtHead(ctx context.Context, owner, repo string, number int, expectedHead string) (*ReviewReport, error) {
	if err := github.ValidateCommitOID(expectedHead); err != nil {
		return nil, fmt.Errorf("invalid expected head commit OID: %w", err)
	}

	pr, err := e.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch PR: %w", err)
	}

	if err := github.ValidateCommitOID(pr.BaseSHA); err != nil {
		return nil, fmt.Errorf("invalid PR base commit OID: %w", err)
	}
	if err := github.ValidateCommitOID(pr.HeadSHA); err != nil {
		return nil, fmt.Errorf("invalid PR head commit OID: %w", err)
	}

	if pr.HeadSHA != expectedHead {
		return nil, fmt.Errorf("expected head SHA %s does not match PR head SHA %s", expectedHead, pr.HeadSHA)
	}

	diff, err := e.gh.GetDiffAtCommits(ctx, owner, repo, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch diff at commits: %w", err)
	}

	report, err := e.executeAgentReview(ctx, pr, diff)
	if err != nil {
		return nil, err
	}
	if report.HeadSHA != expectedHead {
		return nil, fmt.Errorf("report head SHA %s does not match expected head %s", report.HeadSHA, expectedHead)
	}
	return report, nil
}

func (e *Engine) ReviewPR(ctx context.Context, owner, repo string, number int) (*ReviewReport, error) {
	pr, err := e.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch PR: %w", err)
	}

	diff, err := e.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch diff: %w", err)
	}
	return e.executeReview(ctx, pr, diff)
}

func (e *Engine) executeAgentReview(ctx context.Context, pr *github.PRDetails, rawDiff string) (*ReviewReport, error) {
	owner := pr.Owner
	repo := pr.Repo
	number := pr.Number

	// Parse diff into change inventory and coverage ledger
	opts := diff.ParseOptionsFromConfig(e.cfg)
	inv := diff.Parse(rawDiff, opts)
	ledger := diff.NewCoverageLedger(inv)

	generalComments, threads, err := e.gh.GetComments(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comments: %w", err)
	}

	var verificationSummary = "Sandbox verification skipped."
	var verificationStatus sandbox.VerificationStatus = sandbox.StatusUnavailable
	var verificationReason string
	sandboxVerified := false

	var snap *sandbox.Snapshot
	var cleanup func()
	var cleaned bool

	cleanOnce := func() {
		if !cleaned && cleanup != nil {
			cleaned = true
			cleanup()
		}
	}
	defer cleanOnce()

	if e.cfg.EnableSandbox && e.sandbox != nil {
		snapshot, snapCleanup, err := e.sandbox.PrepareSnapshot(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
		if err == nil {
			snap = snapshot
			cleanup = snapCleanup

			// Validate snapshot before exploration and verification
			if err := snap.Validate(); err != nil {
				cleanOnce()
				return nil, fmt.Errorf("snapshot validation failed: %w", err)
			}
			if snap.CommitSHA != pr.HeadSHA {
				cleanOnce()
				return nil, fmt.Errorf("snapshot commit SHA %s does not match PR head SHA %s", snap.CommitSHA, pr.HeadSHA)
			}

			// Run sandbox verification
			verReport, err := e.sandbox.RunSnapshot(ctx, snap)
			if err == nil {
				verificationStatus = verReport.Status
				verificationReason = verReport.Reason
				verificationSummary = verReport.Summary
				if verReport.Status == sandbox.StatusPassed {
					sandboxVerified = true
				} else {
					sandboxVerified = false
					if verReport.Reason != "" {
						verificationSummary = fmt.Sprintf("[%s] %s: %s", verReport.Status, verReport.Summary, verReport.Reason)
					}
				}
				for _, res := range verReport.Results {
					if !res.Passed {
						sandboxVerified = false
						verificationSummary += fmt.Sprintf("\nCommand `%s` failed (exit %d):\n```\n%s%s\n```",
							res.Command, res.ExitCode, res.Stdout, res.Stderr)
					}
				}
			} else {
				verificationStatus = sandbox.StatusUnavailable
				verificationReason = err.Error()
				verificationSummary = fmt.Sprintf("Sandbox verification unavailable: %v", err)
			}
		} else {
			verificationStatus = sandbox.StatusUnavailable
			verificationReason = err.Error()
			verificationSummary = fmt.Sprintf("Sandbox verification unavailable: %v", err)
		}
	}

	// Run agent loop over change inventory and held snapshot
	agent := NewReviewerAgent(e.cfg, e.llm)
	agentRes, err := agent.Run(ctx, pr, inv, ledger, snap, verificationSummary, verificationStatus, generalComments, threads)
	cleanOnce() // Clean up snapshot and permit immediately after loop finishes
	if err != nil {
		return nil, err
	}

	// Deduplicate findings against existing comments
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
	if agentRes.Output != nil {
		for _, f := range agentRes.Output.Findings {
			fp := dedup.ComputeFingerprint(f.File, f.Line, f.Title, f.Severity)
			if existingFPs[fp] {
				skippedCount++
				continue
			}
			uniqueFindings = append(uniqueFindings, f)
		}
	}

	var score int
	var summary string
	var commentFollowups []CommentTracking
	if agentRes.Output != nil {
		score = agentRes.Output.Score
		summary = agentRes.Output.Summary
		commentFollowups = agentRes.Output.CommentFollowups
	}

	report := &ReviewReport{
		PRNumber:            number,
		PRTitle:             pr.Title,
		HeadSHA:             pr.HeadSHA,
		Score:               score,
		Summary:             summary,
		VerificationSummary: verificationSummary,
		VerificationStatus:  verificationStatus,
		VerificationReason:  verificationReason,
		RulesSource:         "",
		DeduplicatedCount:   skippedCount,
		CommentFollowups:    commentFollowups,
		Findings:            uniqueFindings,
		Ledger:              agentRes.Ledger,
	}

	if sandboxVerified {
		report.Suggestions = BuildInlineSuggestions(rawDiff, uniqueFindings)
	}

	report.RawMarkdown = FormatReportMarkdown(report)
	return report, nil
}

func (e *Engine) executeReview(ctx context.Context, pr *github.PRDetails, diff string) (*ReviewReport, error) {
	owner := pr.Owner
	repo := pr.Repo
	number := pr.Number

	const maxDiffSize = 120000 // ~120KB, roughly 30k tokens
	if len(diff) > maxDiffSize {
		diff = diff[:maxDiffSize] + "\n\n... [diff truncated, exceeded size limit] ..."
	}

	generalComments, threads, err := e.gh.GetComments(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comments: %w", err)
	}

	// 4. Sandbox Verification (Optional/Configurable)
	var verificationSummary = "Sandbox verification skipped."
	var verificationStatus sandbox.VerificationStatus = sandbox.StatusUnavailable
	var verificationReason string
	sandboxVerified := false
	if e.cfg.EnableSandbox && e.sandbox != nil {
		snapshot, cleanup, err := e.sandbox.PrepareSnapshot(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
		if err == nil {
			var cleaned bool
			cleanOnce := func() {
				if !cleaned && cleanup != nil {
					cleaned = true
					cleanup()
				}
			}
			defer cleanOnce()

			verReport, err := e.sandbox.RunSnapshot(ctx, snapshot)
			cleanOnce() // Immediate post-verification snapshot cleanup before completion request

			if err == nil {
				verificationStatus = verReport.Status
				verificationReason = verReport.Reason
				verificationSummary = verReport.Summary
				if verReport.Status == sandbox.StatusPassed {
					sandboxVerified = true
				} else {
					sandboxVerified = false
					if verReport.Reason != "" {
						verificationSummary = fmt.Sprintf("[%s] %s: %s", verReport.Status, verReport.Summary, verReport.Reason)
					}
				}
				// If tests or build failed, append stderr/stdout snippet
				for _, res := range verReport.Results {
					if !res.Passed {
						sandboxVerified = false
						verificationSummary += fmt.Sprintf("\nCommand `%s` failed (exit %d):\n```\n%s%s\n```",
							res.Command, res.ExitCode, res.Stdout, res.Stderr)
					}
				}
			} else {
				verificationStatus = sandbox.StatusUnavailable
				verificationReason = err.Error()
				verificationSummary = fmt.Sprintf("Sandbox verification unavailable: %v", err)
			}
		} else {
			verificationStatus = sandbox.StatusUnavailable
			verificationReason = err.Error()
			verificationSummary = fmt.Sprintf("Sandbox verification unavailable: %v", err)
		}
	}

	// 5. Build Prompts
	systemPrompt := buildSystemPrompt(verificationStatus)
	userPrompt := buildUserPrompt(pr, diff, generalComments, threads, verificationSummary, verificationStatus)

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
		VerificationStatus:  verificationStatus,
		VerificationReason:  verificationReason,
		RulesSource:         "",
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

func parseVerificationStatus(status any) sandbox.VerificationStatus {
	switch v := status.(type) {
	case bool:
		if v {
			return sandbox.StatusPassed
		}
		return sandbox.StatusUnavailable
	case sandbox.VerificationStatus:
		return v
	case string:
		return sandbox.VerificationStatus(v)
	default:
		return sandbox.StatusUnavailable
	}
}

func buildSystemPrompt(status any) string {
	st := parseVerificationStatus(status)
	switch st {
	case sandbox.StatusPassed:
		return `You are an elite, highly rigorous AI Code Reviewer.
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

	case sandbox.StatusBuildFailed, sandbox.StatusTestFailed:
		return `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining three critical signals:
1. Live Sandbox Verification (the code was executed in a real isolated runner and FAILED).
2. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
3. The Git Diff.

NOTE: Real live sandbox verification was executed and encountered failures. Do not claim or assume tests passed.

CRITICAL RULES:
- Never hallucinate false bugs. Live sandbox verification proved real failure (compilation or unit tests). Accurately focus on verified build/test defects.
- Do not claim builds or tests passed when live sandbox verification reported failure.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`

	case sandbox.StatusTimeout:
		return `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining three critical signals:
1. Live Sandbox Verification (execution exceeded time limits and timed out).
2. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
3. The Git Diff.

NOTE: Live sandbox verification timed out (exceeded execution budget). Do not claim tests passed.

CRITICAL RULES:
- Never hallucinate false bugs. Do not claim builds or tests passed when execution timed out.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`

	case sandbox.StatusResourceExhausted, sandbox.StatusDiskExhausted:
		return `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining three critical signals:
1. Live Sandbox Verification (execution exceeded memory or storage quota).
2. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
3. The Git Diff.

NOTE: Live sandbox verification exceeded resource limits. Do not claim tests passed.

CRITICAL RULES:
- Never hallucinate false bugs. Do not claim builds or tests passed when resource limits were exceeded.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`

	case sandbox.StatusIncomplete:
		return `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining two critical signals:
1. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
2. The Git Diff.

NOTE: Live sandbox verification was incomplete (unsupported dependencies, missing toolchain, or missing credentials). Do not claim or assume verification succeeded.

CRITICAL RULES:
- Never hallucinate false bugs. Do not claim builds or tests ran to completion or passed when verification was incomplete.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`

	default:
		return `You are an elite, highly rigorous AI Code Reviewer.
Your role is to analyze Pull Requests by combining two critical signals:
1. Existing Discussion History (prior PR comments, reviewer feedback, author clarifications).
2. The Git Diff.

NOTE: Live sandbox verification was skipped or unavailable. Do not claim or assume live sandbox verification was performed.

CRITICAL RULES:
- Never hallucinate false bugs. Do not claim builds or tests ran in a live sandbox when verification was skipped.
- If an existing discussion thread shows a concern was already acknowledged, discussed, or dismissed by the author/reviewer, DO NOT repeat it as a new issue.
- If a reviewer previously requested a fix in a comment thread, verify whether the diff actually satisfies that request.
- Focus strictly on real defects: race conditions, concurrency bugs, nil/null pointer exceptions, resource leaks, breaking API contracts, security flaws, and performance regressions.
- Set suggested_code only when it is a small, exact, safe replacement for one changed line. Otherwise omit it.
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`
	}
}

func buildUserPrompt(pr *github.PRDetails, diff string, comments []github.Comment, threads []github.DiscussionThread, verification string, status any) string {
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
	case sandbox.StatusTimeout:
		b.WriteString("### Real Sandbox Environment Verification (Timeout):\n")
	case sandbox.StatusDiskExhausted, sandbox.StatusResourceExhausted:
		b.WriteString("### Real Sandbox Environment Verification (Resource Exhausted):\n")
	case sandbox.StatusIncomplete:
		b.WriteString("### Sandbox Verification (Incomplete):\n")
	case sandbox.StatusUnsupportedLanguage:
		b.WriteString("### Sandbox Verification (Unsupported Language):\n")
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

	cov := r.GetCoverage()
	isPartial := cov != nil && cov.State == CoverageStatePartial

	if isPartial {
		sb.WriteString(fmt.Sprintf("> ⚠️ **Partial Review Warning:** Review coverage is incomplete (%d of %d hunks examined, %d skipped). Unexamined changes have not been verified and are not cleared for merge.\n\n",
			cov.ExaminedCount, cov.TotalHunks, cov.SkippedCount))
	}

	if cov != nil {
		sb.WriteString("### 📊 Review Coverage\n")
		if isPartial {
			sb.WriteString(fmt.Sprintf("- **Status:** Partial (%d/%d hunks examined, %d skipped)\n",
				cov.ExaminedCount, cov.TotalHunks, cov.SkippedCount))
		} else {
			sb.WriteString(fmt.Sprintf("- **Status:** Full (%d/%d hunks examined)\n",
				cov.ExaminedCount, cov.TotalHunks))
		}

		const maxItems = 25

		if len(cov.ExaminedHunks) > 0 {
			sb.WriteString("- **Examined Hunks:**\n")
			limit := len(cov.ExaminedHunks)
			if limit > maxItems {
				limit = maxItems
			}
			for i := 0; i < limit; i++ {
				sb.WriteString(fmt.Sprintf("  - `%s`\n", cov.ExaminedHunks[i]))
			}
			if len(cov.ExaminedHunks) > maxItems {
				sb.WriteString(fmt.Sprintf("  - *... and %d more examined hunks (%d total examined)*\n",
					len(cov.ExaminedHunks)-maxItems, cov.ExaminedCount))
			}
		}

		if len(cov.SkippedHunks) > 0 {
			sb.WriteString("- **Skipped Hunks:**\n")
			limit := len(cov.SkippedHunks)
			if limit > maxItems {
				limit = maxItems
			}
			for i := 0; i < limit; i++ {
				item := cov.SkippedHunks[i]
				reason := item.Reason
				if reason == "" {
					reason = "not examined"
				}
				sb.WriteString(fmt.Sprintf("  - `%s`: %s\n", item.ID, reason))
			}
			if len(cov.SkippedHunks) > maxItems {
				sb.WriteString(fmt.Sprintf("  - *... and %d more skipped hunks (%d total skipped)*\n",
					len(cov.SkippedHunks)-maxItems, len(cov.SkippedHunks)))
			}
		}

		if len(cov.Omissions) > 0 {
			sb.WriteString("- **Omissions & Unsupported Changes:**\n")
			limit := len(cov.Omissions)
			if limit > maxItems {
				limit = maxItems
			}
			for i := 0; i < limit; i++ {
				item := cov.Omissions[i]
				sb.WriteString(fmt.Sprintf("  - `%s`: %s\n", item.ID, item.Reason))
			}
			if len(cov.Omissions) > maxItems {
				sb.WriteString(fmt.Sprintf("  - *... and %d more omissions (%d total omissions)*\n",
					len(cov.Omissions)-maxItems, len(cov.Omissions)))
			}
		}
		sb.WriteString("\n")
	}

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

	hasClassified := r.Classification != nil || len(r.PersistingFindings) > 0 || len(r.FixedFindings) > 0 || len(r.NewFindings) > 0
	if hasClassified {
		var persisting []PersistingFinding
		var fixed []FixedFinding
		var newFindings []Finding
		if r.Classification != nil {
			persisting = r.Classification.Persisting
			fixed = r.Classification.Fixed
			newFindings = r.Classification.New
		}
		if len(r.PersistingFindings) > 0 {
			persisting = r.PersistingFindings
		}
		if len(r.FixedFindings) > 0 {
			fixed = r.FixedFindings
		}
		if len(r.NewFindings) > 0 {
			newFindings = r.NewFindings
		}

		if len(fixed) > 0 {
			sb.WriteString("### 🛠️ Resolved Findings\n")
			for _, fix := range fixed {
				shaNote := ""
				if fix.PriorSHA != "" {
					shaShort := fix.PriorSHA
					if len(shaShort) > 8 {
						shaShort = shaShort[:8]
					}
					shaNote = fmt.Sprintf(" (addressed since `%s`)", shaShort)
				}
				sb.WriteString(fmt.Sprintf("- ✅ **[RESOLVED]** %s (`%s`)%s\n", fix.Title, fix.File, shaNote))
			}
			sb.WriteString("\n")
		}

		if len(persisting) > 0 {
			sb.WriteString("### 🔄 Persisting Findings\n")
			for _, p := range persisting {
				f := p.Finding
				sevIcon := "⚠️"
				if f.Severity == "CRITICAL" {
					sevIcon = "🚨"
				} else if f.Severity == "NOTE" {
					sevIcon = "💡"
				}
				fp := dedup.ComputeFingerprint(f.File, f.Line, f.Title, f.Severity)
				sb.WriteString(fmt.Sprintf("#### %s [%s] %s (`%s:%d`)\n", sevIcon, f.Severity, f.Title, f.File, f.Line))
				sb.WriteString(fmt.Sprintf("<!-- pr-review-go:fingerprint=%s -->\n", fp))
				priorSHAShort := p.PriorSHA
				if len(priorSHAShort) > 8 {
					priorSHAShort = priorSHAShort[:8]
				}
				sb.WriteString(fmt.Sprintf("> *Previously reported at `%s:%d` in commit `%s`*\n\n", p.PriorFile, p.PriorLine, priorSHAShort))
				sb.WriteString(fmt.Sprintf("%s\n\n", f.Description))
				if f.Suggestion != "" {
					sb.WriteString(fmt.Sprintf("> **Suggestion:** %s\n\n", f.Suggestion))
				}
			}
		}

		if len(newFindings) > 0 {
			sb.WriteString("### 🆕 New Findings\n")
			for _, f := range newFindings {
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

		if len(persisting) == 0 && len(newFindings) == 0 {
			if len(fixed) > 0 {
				sb.WriteString("### 🎯 Findings\nAll prior findings were resolved! No new issues detected.\n\n")
			} else {
				sb.WriteString("### 🎯 Findings\nNo critical bugs or defects detected.\n\n")
			}
		}
	} else if len(r.Findings) == 0 {
		if r.DeduplicatedCount > 0 {
			if isPartial {
				sb.WriteString(fmt.Sprintf("### 🎯 Findings\nAll %d detected issues were already reported previously and have been deduplicated.\n\n*Note: Review coverage was partial; unexamined changes have not been verified.*\n\n", r.DeduplicatedCount))
			} else {
				sb.WriteString(fmt.Sprintf("### 🎯 Findings\nAll %d detected issues were already reported previously and have been deduplicated.\n\n", r.DeduplicatedCount))
			}
		} else {
			if isPartial {
				sb.WriteString("### 🎯 Findings\nNo critical bugs or defects detected in examined files. (Review was partial; unexamined changes have not been verified and are not cleared for merge.)\n\n")
			} else {
				sb.WriteString("### 🎯 Findings\nNo critical bugs or defects detected. Looks ready to merge!\n\n")
			}
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
