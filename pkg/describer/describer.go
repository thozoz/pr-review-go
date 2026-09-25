package describer

import (
	"context"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

const marker = "<!-- pr-review-go:description -->"

type Describer struct {
	gh  *github.Client
	llm *llm.Client
}

func NewDescriber(cfg *config.Config) *Describer {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Describer{
		gh:  github.NewClient(cfg.GitHubToken),
		llm: llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// RunAndUpdate generates a compact PR description and appends it without changing author text.
// It is idempotent: an existing generated section is left untouched.
func (d *Describer) RunAndUpdate(ctx context.Context, owner, repo string, number int) (bool, error) {
	pr, err := d.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("failed to get PR: %w", err)
	}
	if strings.Contains(pr.Body, marker) {
		return false, nil
	}

	diff, err := d.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("failed to get PR diff: %w", err)
	}

	description, err := d.Generate(ctx, pr, diff)
	if err != nil {
		return false, err
	}
	body := appendDescription(pr.Body, description)
	if err := d.gh.UpdatePRBody(ctx, owner, repo, number, body); err != nil {
		return false, fmt.Errorf("failed to update PR description: %w", err)
	}
	return true, nil
}

func (d *Describer) Generate(ctx context.Context, pr *github.PRDetails, diff string) (string, error) {
	const systemPrompt = `You write concise, professional GitHub Pull Request descriptions.

Output Markdown only, using exactly these sections:
## Purpose
One short paragraph stating what this PR changes and why.

## Walkthrough
A table with columns File and Change. Include only meaningfully changed files and describe why each changed.

Do not include Mermaid, diagrams, generic praise, test claims, or sections not listed above.`

	if len(diff) > 80000 {
		diff = diff[:80000] + "\n...[diff truncated]..."
	}
	userPrompt := fmt.Sprintf("PR title: %s\nAuthor: %s\nExisting author description:\n%s\n\nDiff:\n```diff\n%s\n```",
		pr.Title, pr.Author, strings.TrimSpace(pr.Body), diff)

	output, err := d.llm.ChatCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("llm description generation failed: %w", err)
	}
	return strings.TrimSpace(output), nil
}

func appendDescription(authorBody, generated string) string {
	section := marker + "\n" + strings.TrimSpace(generated)
	if strings.TrimSpace(authorBody) == "" {
		return section
	}
	return strings.TrimSpace(authorBody) + "\n\n---\n\n" + section
}
