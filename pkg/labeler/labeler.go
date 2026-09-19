package labeler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

type LabelDef struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DefaultLabels mimics PR-Agent's default label categorization
var DefaultLabels = []LabelDef{
	{Key: "bug_fix", Name: "Bug fix", Description: "Fixes a bug or defect in existing code"},
	{Key: "enhancement", Name: "Enhancement", Description: "Introduces a new feature or functional improvement"},
	{Key: "tests", Name: "Tests", Description: "Adds or updates test suites, fixtures, or benchmarks"},
	{Key: "documentation", Name: "Documentation", Description: "Updates documentation, markdown files, or code comments"},
	{Key: "refactoring", Name: "Refactoring", Description: "Code refactoring that does not change external behavior"},
	{Key: "maintenance", Name: "Maintenance", Description: "Build scripts, dependencies, CI/CD pipelines, or configuration"},
}

type Labeler struct {
	cfg *config.Config
	gh  *github.Client
	llm *llm.Client
}

func NewLabeler(cfg *config.Config) *Labeler {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Labeler{
		cfg: cfg,
		gh:  github.NewClient(cfg.GitHubToken),
		llm: llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

type labelOutput struct {
	Labels []string `json:"labels"`
}

// GenerateLabels inspects PR title, description, and diff, returning matched labels
func (l *Labeler) GenerateLabels(ctx context.Context, pr *github.PRDetails, diff string) ([]string, error) {
	systemPrompt := buildSystemPrompt()
	userPrompt := buildUserPrompt(pr, diff)

	rawResponse, err := l.llm.ChatCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("llm label generation failed: %w", err)
	}

	selected, err := parseLabelsJSON(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to parse label response: %w, raw: %s", err, rawResponse)
	}

	// Map returned keys or names to canonical label names
	canonical := resolveToCanonical(selected)
	return canonical, nil
}

// RunAndApply fetches the diff and directly applies labels to the GitHub PR
func (l *Labeler) RunAndApply(ctx context.Context, owner, repo string, number int) ([]string, error) {
	pr, err := l.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR: %w", err)
	}

	diff, err := l.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR diff: %w", err)
	}

	labels, err := l.GenerateLabels(ctx, pr, diff)
	if err != nil {
		return nil, err
	}

	if len(labels) > 0 {
		if err := l.gh.AddLabels(ctx, owner, repo, number, labels); err != nil {
			return nil, fmt.Errorf("failed to apply labels to GitHub: %w", err)
		}
	}

	return labels, nil
}

func buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are PR-Reviewer, a language model designed to inspect a Git Pull Request and categorize it with labels.\n")
	sb.WriteString("Thoroughly evaluate the PR title, description, and Git diff against the following available labels:\n\n")

	for _, l := range DefaultLabels {
		sb.WriteString(fmt.Sprintf("- `%s`: %s\n", l.Name, l.Description))
	}

	sb.WriteString("\nSelect ONLY the relevant labels that accurately reflect the contents of this PR.\n")
	sb.WriteString("Respond with a valid JSON object matching this exact format, with no markdown codeblocks or extra text:\n")
	sb.WriteString("{\"labels\": [\"Bug fix\", \"Tests\"]}\n")

	return sb.String()
}

func buildUserPrompt(pr *github.PRDetails, diff string) string {
	var sb strings.Builder
	sb.WriteString("## PR Info\n")
	sb.WriteString(fmt.Sprintf("Title: %s\nBranch: %s\nAuthor: %s\n\n", pr.Title, pr.HeadRef, pr.Author))

	if pr.Body != "" {
		sb.WriteString("### Description:\n")
		sb.WriteString(strings.TrimSpace(pr.Body) + "\n\n")
	}

	sb.WriteString("### Git Diff:\n```diff\n")
	// If diff is too huge for simple labeling, cap it around 40,000 characters
	cappedDiff := diff
	if len(cappedDiff) > 40000 {
		cappedDiff = cappedDiff[:40000] + "\n\n...[diff truncated for labeling]..."
	}
	sb.WriteString(cappedDiff)
	sb.WriteString("\n```\n")

	return sb.String()
}

func parseLabelsJSON(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	
	// Try to find JSON in the response
	jsonStart := strings.Index(trimmed, "{")
	jsonEnd := strings.LastIndex(trimmed, "}")
	
	if jsonStart >= 0 && jsonEnd > jsonStart {
		trimmed = trimmed[jsonStart : jsonEnd+1]
	}

	var out labelOutput
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, err
	}
	return out.Labels, nil
}

func resolveToCanonical(input []string) []string {
	var res []string
	seen := make(map[string]bool)

	for _, item := range input {
		clean := strings.TrimSpace(strings.ToLower(item))
		for _, def := range DefaultLabels {
			if strings.ToLower(def.Name) == clean || strings.ToLower(def.Key) == clean {
				if !seen[def.Name] {
					seen[def.Name] = true
					res = append(res, def.Name)
				}
				break
			}
		}
	}
	return res
}
