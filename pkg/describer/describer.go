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

const (
	marker             = "<!-- pr-review-go:description -->"
	diagramStartMarker = "<!-- pr-review-go:diagram -->"
	diagramEndMarker   = "<!-- /pr-review-go:diagram -->"
)

// TruncationNotice is appended to generated PR descriptions when the diff was truncated at a hunk boundary.
const TruncationNotice = "\n\n> ⚠️ *Note: Diff exceeded size limits and was truncated at a hunk boundary; description is based on partial diff.*"

type Describer struct {
	cfg *config.Config
	gh  *github.Client
	llm *llm.Client
}

func NewDescriber(cfg *config.Config) *Describer {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid config: %v", err))
	}
	return &Describer{
		cfg: cfg,
		gh:  github.NewClientFromConfig(cfg),
		llm: llm.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel),
	}
}

// RunAndUpdate generates a compact PR description and appends it without changing author text.
// It is idempotent: an existing generated section is left untouched unless diagram opt-in replaces the diagram block.
func (d *Describer) RunAndUpdate(ctx context.Context, owner, repo string, number int) (bool, error) {
	return d.RunWithOptions(ctx, owner, repo, number, false)
}

// RunWithOptions runs describer with optional explicit command opt-in for diagrams (D-06).
func (d *Describer) RunWithOptions(ctx context.Context, owner, repo string, number int, commandOptIn bool) (bool, error) {
	pr, err := d.gh.GetPR(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("failed to get PR: %w", err)
	}

	withDiagram := (d.cfg != nil && d.cfg.EnableMermaid) || commandOptIn
	hasDescription := strings.Contains(pr.Body, marker)

	if hasDescription && !withDiagram {
		return false, nil
	}

	diff, err := d.gh.GetRawDiff(ctx, owner, repo, number)
	if err != nil {
		return false, fmt.Errorf("failed to get PR diff: %w", err)
	}

	if hasDescription && withDiagram {
		// Re-run: replace only diagram sub-block or backfill (D-07)
		diagramBlock, err := d.generateDiagramBlock(ctx, pr, diff)
		if err != nil {
			return false, err
		}
		if diagramBlock == "" {
			return false, nil
		}

		var newBody string
		if strings.Contains(pr.Body, diagramStartMarker) {
			newBody = replaceDiagramBlock(pr.Body, diagramBlock)
		} else {
			newBody = backfillDiagramBlock(pr.Body, diagramBlock)
		}

		if newBody == pr.Body {
			return false, nil
		}
		if err := d.gh.UpdatePRBody(ctx, owner, repo, number, newBody); err != nil {
			return false, fmt.Errorf("failed to update PR description diagram: %w", err)
		}
		return true, nil
	}

	// Fresh generation: generate prose (and diagram if opted in)
	description, diagram, _, err := d.GenerateWithDiagram(ctx, pr, diff, withDiagram)
	if err != nil {
		return false, err
	}

	fullGenerated := description
	if diagram != "" {
		fullGenerated = fullGenerated + "\n\n" + formatDiagramBlock(diagram)
	}

	body := appendDescription(pr.Body, fullGenerated)
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
	desc, _, trunc, err := d.GenerateWithDiagram(ctx, pr, diff, false)
	return desc, trunc, err
}

// GenerateWithDiagram generates a description and optional Mermaid diagram (D-06, D-08).
func (d *Describer) GenerateWithDiagram(ctx context.Context, pr *github.PRDetails, diff string, withDiagram bool) (string, string, bool, error) {
	systemPrompt := `You write concise, professional GitHub Pull Request descriptions.

Output Markdown only, using exactly these sections:
## Purpose
One short paragraph stating what this PR changes and why.

## Walkthrough
A table with columns File and Change. Include only meaningfully changed files and describe why each changed.

Do not include Mermaid, diagrams, generic praise, test claims, or sections not listed above.`

	inv := diffpkg.Parse(diff)
	window := inv.Window(80000)
	diffText := window.Content
	if window.Truncated {
		diffText += "\n...[diff truncated at hunk boundary]..."
	}
	userPrompt := fmt.Sprintf("PR title: %s\nAuthor: %s\nExisting author description:\n%s\n\nDiff:\n```diff\n%s\n```",
		pr.Title, pr.Author, strings.TrimSpace(pr.Body), diffText)

	output, err := d.llm.ChatCompletion(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", "", window.Truncated, fmt.Errorf("llm description generation failed: %w", err)
	}

	result := strings.TrimSpace(output)
	if window.Truncated {
		result += TruncationNotice
	}

	var diagram string
	if withDiagram {
		var filePaths []string
		for _, f := range inv.Files {
			if f.Path != "" {
				filePaths = append(filePaths, f.Path)
			}
		}
		diagInput := DiagramInput{
			Files:     filePaths,
			Truncated: window.Truncated,
		}
		threshold := config.DefaultMermaidDirectionThreshold
		maxNodes := config.DefaultMermaidMaxNodes
		maxEdges := config.DefaultMermaidMaxEdges
		maxBytes := int64(config.DefaultMermaidMaxBytes)
		if d.cfg != nil {
			if d.cfg.MermaidDirectionThreshold > 0 {
				threshold = d.cfg.MermaidDirectionThreshold
			}
			if d.cfg.MermaidMaxNodes > 0 {
				maxNodes = d.cfg.MermaidMaxNodes
			}
			if d.cfg.MermaidMaxEdges > 0 {
				maxEdges = d.cfg.MermaidMaxEdges
			}
			if d.cfg.MermaidMaxBytes > 0 {
				maxBytes = d.cfg.MermaidMaxBytes
			}
		}

		diag, diagErr := GenerateDiagram(ctx, d.llm, diagInput, threshold, maxNodes, maxEdges, maxBytes, MaxDiagramCalls)
		if diagErr == nil {
			diagram = diag
		}
	}

	return result, diagram, window.Truncated, nil
}

func (d *Describer) generateDiagramBlock(ctx context.Context, pr *github.PRDetails, diff string) (string, error) {
	inv := diffpkg.Parse(diff)
	window := inv.Window(80000)
	var filePaths []string
	for _, f := range inv.Files {
		if f.Path != "" {
			filePaths = append(filePaths, f.Path)
		}
	}
	diagInput := DiagramInput{
		Files:     filePaths,
		Truncated: window.Truncated,
	}
	threshold := config.DefaultMermaidDirectionThreshold
	maxNodes := config.DefaultMermaidMaxNodes
	maxEdges := config.DefaultMermaidMaxEdges
	maxBytes := int64(config.DefaultMermaidMaxBytes)
	if d.cfg != nil {
		if d.cfg.MermaidDirectionThreshold > 0 {
			threshold = d.cfg.MermaidDirectionThreshold
		}
		if d.cfg.MermaidMaxNodes > 0 {
			maxNodes = d.cfg.MermaidMaxNodes
		}
		if d.cfg.MermaidMaxEdges > 0 {
			maxEdges = d.cfg.MermaidMaxEdges
		}
		if d.cfg.MermaidMaxBytes > 0 {
			maxBytes = d.cfg.MermaidMaxBytes
		}
	}

	diag, err := GenerateDiagram(ctx, d.llm, diagInput, threshold, maxNodes, maxEdges, maxBytes, MaxDiagramCalls)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diag) == "" {
		return "", nil
	}
	return formatDiagramBlock(diag), nil
}

func formatDiagramBlock(diagram string) string {
	if strings.TrimSpace(diagram) == "" {
		return ""
	}
	return fmt.Sprintf("%s\n### Architecture & Flow Diagram\n\n%s\n%s",
		diagramStartMarker, strings.TrimSpace(diagram), diagramEndMarker)
}

func replaceDiagramBlock(body, newDiagramBlock string) string {
	startIdx := strings.Index(body, diagramStartMarker)
	endIdx := strings.Index(body, diagramEndMarker)
	if startIdx >= 0 && endIdx >= startIdx {
		endIdx += len(diagramEndMarker)
		return body[:startIdx] + newDiagramBlock + body[endIdx:]
	}
	return body
}

func backfillDiagramBlock(body, newDiagramBlock string) string {
	return strings.TrimRight(body, "\n") + "\n\n" + newDiagramBlock
}

func appendDescription(authorBody, generated string) string {
	section := marker + "\n" + strings.TrimSpace(generated)
	if strings.TrimSpace(authorBody) == "" {
		return section
	}
	return strings.TrimSpace(authorBody) + "\n\n---\n\n" + section
}
