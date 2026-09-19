package summarizer

import (
	"context"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

type Summarizer struct {
	cfg *config.Config
	gh  *github.Client
	llm *llm.Client
}

func NewSummarizer(cfg *config.Config) *Summarizer {
	return &Summarizer{
		cfg: cfg,
		gh:  github.NewClient(cfg.GitHubToken),
		llm: llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// SummarizeDiscussions compiles all PR comments into an executive summary
func (s *Summarizer) SummarizeDiscussions(ctx context.Context, owner, repo string, number int) (string, error) {
	pr, err := s.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("failed to get PR: %w", err)
	}

	generalComments, threads, err := s.gh.GetComments(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("failed to get comments: %w", err)
	}

	if len(generalComments) == 0 && len(threads) == 0 {
		return fmt.Sprintf("## 📝 PR Discussion Summary: #%d (%s)\n\nNo discussion comments or review threads have been posted yet.", number, pr.Title), nil
	}

	systemPrompt := `You are an elite Technical Program Manager and Code Review Facilitator.
Your job is to read all discussions, reviewer feedback, inline code comments, and author replies on a GitHub Pull Request, then produce a clear, objective, and structured summary.

Structure your markdown response as follows:
## 📝 PR Discussion & Review Summary: [PR Title]

### 🎯 Key Consensus & Agreed Decisions
- Bullet points of what reviewers and author agreed upon.

### ⚠️ Open / Unresolved Concerns
- Bullet points of concerns or requested changes that are still pending or debated. (Say "None" if all resolved).

### 📋 Action Items
- Next concrete steps needed before merge.`

	var userPrompt strings.Builder
	userPrompt.WriteString(fmt.Sprintf("PR Title: %s\nAuthor: %s\nDescription:\n%s\n\n", pr.Title, pr.Author, pr.Body))
	userPrompt.WriteString("### Discussion Comments:\n")
	for _, c := range generalComments {
		userPrompt.WriteString(fmt.Sprintf("- [@%s]: %s\n", c.User, c.Body))
	}

	userPrompt.WriteString("\n### Inline Code Review Threads:\n")
	for _, th := range threads {
		userPrompt.WriteString(fmt.Sprintf("\n#### File `%s` (Line %d):\n", th.Path, th.Line))
		if th.DiffHunk != "" {
			userPrompt.WriteString(fmt.Sprintf("```\n%s\n```\n", th.DiffHunk))
		}
		for _, tc := range th.Comments {
			userPrompt.WriteString(fmt.Sprintf("  - [@%s]: %s\n", tc.User, tc.Body))
		}
	}

	summary, err := s.llm.ChatCompletion(ctx, systemPrompt, userPrompt.String())
	if err != nil {
		return "", fmt.Errorf("llm summary call failed: %w", err)
	}

	return strings.TrimSpace(summary), nil
}

// RunAndPost generates discussion summary and posts it directly to GitHub PR
func (s *Summarizer) RunAndPost(ctx context.Context, owner, repo string, number int) (string, error) {
	summary, err := s.SummarizeDiscussions(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}

	if err := s.gh.PostComment(ctx, owner, repo, number, summary); err != nil {
		return "", fmt.Errorf("failed to post summary to GitHub: %w", err)
	}

	return summary, nil
}
