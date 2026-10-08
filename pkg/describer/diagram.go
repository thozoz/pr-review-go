package describer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/thozoz/pr-review-go/pkg/llm"
)

const (
	// MaxDiagramCalls is the hard ceiling on AI calls for chunked diagram generation (D-08).
	MaxDiagramCalls = 4
	// DefaultDiagramChunkSize is the maximum number of files analyzed per chunk.
	DefaultDiagramChunkSize = 10
)

// SanitizeMermaid extracts only the fenced ```mermaid ... ``` code block,
// strips any HTML/script or other tags, enforces maxBytes bound, and returns an empty string
// if no valid mermaid diagram is found (D-08, T-04-04-02).
func SanitizeMermaid(raw string, maxBytes int64) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	// Regex to extract ```mermaid ... ``` block
	re := regexp.MustCompile("(?s)```mermaid\\s*\\n?(.*?)\\n?```")
	matches := re.FindStringSubmatch(trimmed)

	var content string
	if len(matches) >= 2 {
		content = strings.TrimSpace(matches[1])
	} else {
		// If the model emitted raw flowchart/graph without backticks
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "flowchart") || strings.HasPrefix(lower, "graph") || strings.HasPrefix(lower, "sequencediagram") || strings.HasPrefix(lower, "classDiagram") {
			content = trimmed
		}
	}

	if content == "" {
		return ""
	}

	// Strip any dangerous HTML / script tags
	stripScript := regexp.MustCompile("(?i)<script.*?>.*?</script>")
	content = stripScript.ReplaceAllString(content, "")
	stripHTML := regexp.MustCompile("(?i)<.*?>")
	content = stripHTML.ReplaceAllString(content, "")

	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}

	// Wrap in canonical mermaid code block
	wrapped := fmt.Sprintf("```mermaid\n%s\n```", content)
	if maxBytes > 0 && int64(len(wrapped)) > maxBytes {
		return ""
	}

	return wrapped
}

// AdaptiveDirection returns "TD" (top-down) if longest chain or item count > threshold; otherwise "LR" (D-08).
func AdaptiveDirection(nodeCount int, threshold int) string {
	if threshold <= 0 {
		threshold = 5
	}
	if nodeCount > threshold {
		return "TD"
	}
	return "LR"
}

// DiagramInput contains structural file changes and hunk headers for diagram generation.
type DiagramInput struct {
	Files       []string // changed file paths
	HunkHeaders []string // structural summary (e.g. "pkg/auth: Add token check")
	Truncated   bool     // whether input exceeded diff window
}

// GenerateDiagram produces a sanitized Mermaid diagram with adaptive direction,
// chunked generation bounded to at most 4 AI calls, unprocessed-files list, and coverage footer (D-08).
func GenerateDiagram(
	ctx context.Context,
	llmClient *llm.Client,
	input DiagramInput,
	threshold int,
	maxNodes int,
	maxEdges int,
	maxBytes int64,
	callBudget int,
) (string, error) {
	if len(input.Files) == 0 {
		return "", nil
	}

	if callBudget <= 0 || callBudget > MaxDiagramCalls {
		callBudget = MaxDiagramCalls
	}

	dir := AdaptiveDirection(len(input.Files), threshold)

	// Partition files into chunks
	chunkSize := DefaultDiagramChunkSize
	totalFiles := len(input.Files)
	var processedFiles []string
	var unprocessedFiles []string

	callsMade := 0
	var diagramParts []string

	for i := 0; i < totalFiles; i += chunkSize {
		end := i + chunkSize
		if end > totalFiles {
			end = totalFiles
		}

		if callsMade >= callBudget {
			unprocessedFiles = append(unprocessedFiles, input.Files[i:totalFiles]...)
			break
		}

		chunkFiles := input.Files[i:end]
		processedFiles = append(processedFiles, chunkFiles...)
		callsMade++

		systemPrompt := fmt.Sprintf(`You generate clean, professional Mermaid flowcharts for software pull requests.
Use diagram direction: %s.
Use syntax: flowchart %s
Keep the diagram high-level and structural (package/component level).
Max nodes: %d, max edges: %d.
Output ONLY a fenced `+"```mermaid ... ```"+` block. If changes are trivial or inapplicable for a diagram, output nothing.`,
			dir, dir, maxNodes, maxEdges)

		userPrompt := fmt.Sprintf("Changed files in this chunk:\n%s\n\nSummary headers:\n%s",
			strings.Join(chunkFiles, "\n"),
			strings.Join(input.HunkHeaders, "\n"),
		)

		if llmClient != nil {
			resp, err := llmClient.ChatCompletion(ctx, systemPrompt, userPrompt)
			if err != nil {
				return "", err
			}
			sanitized := SanitizeMermaid(resp, maxBytes)
			if sanitized != "" {
				diagramParts = append(diagramParts, sanitized)
			}
		} else {
			// Stub / fallback structural generation when llmClient is nil (test mode)
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("flowchart %s\n", dir))
			for idx, f := range chunkFiles {
				sb.WriteString(fmt.Sprintf("    F%d[%s]\n", idx, f))
			}
			diagramParts = append(diagramParts, fmt.Sprintf("```mermaid\n%s```", sb.String()))
		}
	}

	if len(diagramParts) == 0 {
		return "", nil
	}

	// Use the primary/first diagram or merged diagram
	finalDiagram := diagramParts[0]

	// Append unprocessed files and coverage footer if budget or window was truncated
	var footer strings.Builder
	if len(unprocessedFiles) > 0 {
		footer.WriteString(fmt.Sprintf("\n\n> ⚠️ *Unprocessed files (exceeded diagram budget):* %s",
			strings.Join(unprocessedFiles, ", ")))
	}
	if input.Truncated || len(unprocessedFiles) > 0 {
		footer.WriteString("\n\n> ℹ️ *Note: Diagram reflects partial repository changes within generation budget.*")
	}

	return finalDiagram + footer.String(), nil
}
