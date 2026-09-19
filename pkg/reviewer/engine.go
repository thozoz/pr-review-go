package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
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

	// 2. Fetch Raw Diff
	diff, err := e.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch diff: %w", err)
	}

	// 3. Fetch Comments & Discussions
	generalComments, threads, err := e.gh.GetComments(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comments: %w", err)
	}

	// 4. Sandbox Verification (Optional/Configurable)
	var verificationSummary = "Sandbox verification skipped."
	if e.cfg.EnableSandbox {
		workDir, cleanup, err := e.sandbox.PrepareWorkspace(ctx, pr.CloneURL, pr.HeadRef, pr.HeadSHA)
		if err == nil {
			defer cleanup()
			verReport, err := e.sandbox.VerifyProject(ctx, workDir)
			if err == nil {
				verificationSummary = verReport.Summary
				// If tests or build failed, append stderr/stdout snippet
				for _, res := range verReport.Results {
					if !res.Passed {
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
	systemPrompt := buildSystemPrompt()
	userPrompt := buildUserPrompt(pr, diff, generalComments, threads, verificationSummary)

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

	report := &ReviewReport{
		PRNumber:            number,
		PRTitle:             pr.Title,
		Score:               parsed.Score,
		Summary:             parsed.Summary,
		VerificationSummary: verificationSummary,
		CommentFollowups:    parsed.CommentFollowups,
		Findings:            parsed.Findings,
	}

	report.RawMarkdown = FormatReportMarkdown(report)
	return report, nil
}

func buildSystemPrompt() string {
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
- Output MUST be valid JSON conforming to the schema below. Do not wrap in markdown or add conversational filler.`
}

func buildUserPrompt(pr *github.PRDetails, diff string, comments []github.Comment, threads []github.DiscussionThread, verification string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## PR Info\nTitle: %s\nAuthor: %s\nBase Branch: %s\nHead Branch: %s\n\n",
		pr.Title, pr.Author, pr.BaseRef, pr.HeadRef))

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
      "suggestion": "Add select with ctx.Done() before pushing into the channel."
    }
  ]
}`)

	return b.String()
}

func extractJSONOutput(raw string) (*LLMReviewOutput, error) {
	trimmed := strings.TrimSpace(raw)
	// Strip markdown codeblocks if model included them
	if strings.HasPrefix(trimmed, "```") {
		lines := strings.Split(trimmed, "\n")
		if len(lines) >= 2 {
			if strings.HasPrefix(lines[0], "```") {
				lines = lines[1:]
			}
			if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "```") {
				lines = lines[:len(lines)-1]
			}
			trimmed = strings.Join(lines, "\n")
		}
	}

	var output LLMReviewOutput
	if err := json.Unmarshal([]byte(trimmed), &output); err != nil {
		return nil, err
	}
	return &output, nil
}

func FormatReportMarkdown(r *ReviewReport) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## 🤖 PR Review Report: %s (Score: %d/100)\n\n", r.PRTitle, r.Score))
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
		sb.WriteString("### 🎯 Findings\nNo critical bugs or defects detected. Looks ready to merge!\n")
	} else {
		sb.WriteString("### 🎯 Findings\n")
		for _, f := range r.Findings {
			sevIcon := "⚠️"
			if f.Severity == "CRITICAL" {
				sevIcon = "🚨"
			} else if f.Severity == "NOTE" {
				sevIcon = "💡"
			}

			sb.WriteString(fmt.Sprintf("#### %s [%s] %s (`%s:%d`)\n", sevIcon, f.Severity, f.Title, f.File, f.Line))
			sb.WriteString(fmt.Sprintf("%s\n\n", f.Description))
			if f.Suggestion != "" {
				sb.WriteString(fmt.Sprintf("> **Suggestion:** %s\n\n", f.Suggestion))
			}
		}
	}

	return sb.String()
}
