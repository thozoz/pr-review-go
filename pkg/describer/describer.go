package describer

import (
	"context"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	diffpkg "github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

const marker = "<!-- pr-review-go:description -->"

// TruncationNotice is appended to generated PR descriptions when the diff was truncated at a hunk boundary.
const TruncationNotice = "\n\n> ⚠️ *Note: Diff exceeded size limits and was truncated at a hunk boundary; description is based on partial diff.*"

type Describer struct {
	gh  *github.Client
	llm *llm.Client
}

func NewDescriber(cfg *config.Config) *Describer {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Describer{
		gh:  github.NewClientFromConfig(cfg),
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
	text, _, err := d.GenerateWithStatus(ctx, pr, diff)
	return text, err
}

// GenerateWithStatus generates a PR description and indicates whether diff windowing truncated input.
func (d *Describer) GenerateWithStatus(ctx context.Context, pr *github.PRDetails, diff string) (string, bool, error) {
	const systemPrompt = `You write concise, professional GitHub Pull Request descriptions.

Output Markdown only, using exactly these sections:
## Purpose
One short paragraph stating what this PR changes and why.

## Walkthrough
A table with columns File and Change. Include only meaningfully changed files and describe why each changed.

Do not include Mermaid, diagrams, generic praise, test claims, or sections not listed above.`

	window := diffpkg.Parse(diff).Window(80000)
	diffText := window.Content
	if window.Truncated {
		diffText += "\n...[diff truncated at hunk boundary]..."
	}
	userPrompt := fmt.Sprintf("PR title: %s\nAuthor: %s\nExisting author description:\n%s\n\nDiff:\n```diff\n%s\n```",
		pr.Title, pr.Author, strings.TrimSpace(pr.Body), diffText)

	output, err := d.llm.ChatCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", window.Truncated, fmt.Errorf("llm description generation failed: %w", err)
	}

	result := strings.TrimSpace(output)
	if window.Truncated {
		result += TruncationNotice
	}
	return result, window.Truncated, nil
}

func appendDescription(authorBody, generated string) string {
	section := marker + "\n" + strings.TrimSpace(generated)
	if strings.TrimSpace(authorBody) == "" {
		return section
	}
	return strings.TrimSpace(authorBody) + "\n\n---\n\n" + section
}
