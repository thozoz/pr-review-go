package changelog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

const changelogPath = "CHANGELOG.md"

var validCategories = map[string]bool{
	"Added": true, "Changed": true, "Deprecated": true, "Removed": true, "Fixed": true, "Security": true,
}

type Updater struct {
	gh  *github.Client
	llm *llm.Client
}

type entryOutput struct {
	Category string `json:"category"`
	Entry    string `json:"entry"`
}

func NewUpdater(cfg *config.Config) *Updater {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Updater{
		gh:  github.NewClient(cfg.GitHubToken),
		llm: llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// RunAndPost updates only an unchanged CHANGELOG.md and reports the result on the PR.
func (u *Updater) RunAndPost(ctx context.Context, owner, repo string, number int) error {
	pr, err := u.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("failed to get PR: %w", err)
	}
	diff, err := u.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("failed to get PR diff: %w", err)
	}
	if ChangesChangelog(diff) {
		return u.gh.PostComment(ctx, owner, repo, number, "## 📜 Changelog\n\n`CHANGELOG.md` already changed in this PR; no bot update created.")
	}

	file, err := u.gh.GetFileContent(ctx, owner, repo, changelogPath, pr.HeadRef)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", changelogPath, err)
	}
	entry, err := u.GenerateEntry(ctx, pr, diff)
	if err != nil {
		return err
	}
	updated, err := InsertUnreleasedEntry(file.Content, entry.Category, entry.Entry)
	if err != nil {
		return err
	}
	sha, err := u.gh.UpdateFile(ctx, owner, repo, changelogPath, pr.HeadRef, file.SHA, updated, "docs: update changelog")
	if err != nil {
		return fmt.Errorf("failed to commit changelog: %w", err)
	}
	return u.gh.PostComment(ctx, owner, repo, number, fmt.Sprintf("## 📜 Changelog\n\nUpdated `CHANGELOG.md` in [commit `%s`](https://github.com/%s/%s/commit/%s).", sha, owner, repo, sha))
}

func (u *Updater) GenerateEntry(ctx context.Context, pr *github.PRDetails, diff string) (*entryOutput, error) {
	if len(diff) > 60000 {
		diff = diff[:60000] + "\n...[diff truncated]..."
	}
	const systemPrompt = `Write one Keep a Changelog entry for a Pull Request.
Return strict JSON only: {"category":"Added","entry":"Short user-facing change."}
category must be one of Added, Changed, Deprecated, Removed, Fixed, Security.
entry must be one sentence, no markdown bullet, heading, issue reference, or test-only claim.`
	raw, err := u.llm.ChatCompletion(ctx, systemPrompt, fmt.Sprintf("PR title: %s\nPR body: %s\n\nDiff:\n```diff\n%s\n```", pr.Title, pr.Body, diff))
	if err != nil {
		return nil, fmt.Errorf("llm changelog generation failed: %w", err)
	}
	output, err := parseEntry(raw)
	if err != nil {
		return nil, err
	}
	return output, nil
}

func ChangesChangelog(diff string) bool {
	return strings.Contains(diff, "diff --git a/CHANGELOG.md b/CHANGELOG.md")
}

func InsertUnreleasedEntry(content, category, entry string) (string, error) {
	if !validCategories[category] {
		return "", fmt.Errorf("invalid changelog category: %s", category)
	}
	entry = strings.TrimSpace(entry)
	if entry == "" || strings.ContainsAny(entry, "\r\n") || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, "-") {
		return "", fmt.Errorf("invalid changelog entry")
	}

	lines := strings.Split(content, "\n")
	unreleased := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## [Unreleased]" {
			unreleased = i
			break
		}
	}
	if unreleased == -1 {
		return "", fmt.Errorf("%s must contain a ## [Unreleased] section", changelogPath)
	}
	sectionEnd := len(lines)
	for i := unreleased + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			sectionEnd = i
			break
		}
	}

	heading := "### " + category
	for i := unreleased + 1; i < sectionEnd; i++ {
		if strings.TrimSpace(lines[i]) == heading {
			insertAt := i + 1
			for insertAt < sectionEnd && strings.TrimSpace(lines[insertAt]) == "" {
				insertAt++
			}
			return insertLines(lines, insertAt, "- "+entry), nil
		}
	}
	return insertLines(lines, unreleased+1, "", heading, "", "- "+entry), nil
}

func insertLines(lines []string, index int, values ...string) string {
	result := make([]string, 0, len(lines)+len(values))
	result = append(result, lines[:index]...)
	result = append(result, values...)
	result = append(result, lines[index:]...)
	return strings.Join(result, "\n")
}

func parseEntry(raw string) (*entryOutput, error) {
	raw = strings.TrimSpace(raw)
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		raw = raw[start : end+1]
	}
	var output entryOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, fmt.Errorf("invalid changelog JSON: %w", err)
	}
	if !validCategories[output.Category] {
		return nil, fmt.Errorf("invalid changelog category: %s", output.Category)
	}
	return &output, nil
}
