package describer

import (
	"context"
	"strings"
	"testing"
)

func TestSanitizeMermaid(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		maxBytes int64
		wantHas  string
		wantNil  bool
	}{
		{
			name:    "clean mermaid block",
			input:   "```mermaid\nflowchart TD\n    A --> B\n```",
			wantHas: "flowchart TD\n    A --> B",
		},
		{
			name:    "raw flowchart without backticks",
			input:   "flowchart LR\n    X --> Y",
			wantHas: "flowchart LR\n    X --> Y",
		},
		{
			name:    "strips script and html injection (T-04-04-02)",
			input:   "```mermaid\nflowchart TD\n    A[<script>alert('xss')</script>Safe] --> B<b>Bold</b>\n```",
			wantHas: "A[Safe] --> BBold",
		},
		{
			name:    "rejects non-mermaid code block",
			input:   "```json\n{\"foo\":\"bar\"}\n```",
			wantNil: true,
		},
		{
			name:    "rejects random text without mermaid",
			input:   "Here is some text explaining the architecture.",
			wantNil: true,
		},
		{
			name:     "enforces byte cap",
			input:    "```mermaid\nflowchart TD\n    A --> B\n```",
			maxBytes: 10, // too small
			wantNil:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := SanitizeMermaid(tc.input, tc.maxBytes)
			if tc.wantNil {
				if res != "" {
					t.Fatalf("expected empty result, got %q", res)
				}
				return
			}
			if !strings.Contains(res, tc.wantHas) {
				t.Fatalf("expected %q in result, got %q", tc.wantHas, res)
			}
			if !strings.HasPrefix(res, "```mermaid") || !strings.HasSuffix(res, "```") {
				t.Fatalf("expected result wrapped in mermaid code fence, got %q", res)
			}
		})
	}
}

func TestAdaptiveDirection(t *testing.T) {
	threshold := 5

	if dir := AdaptiveDirection(3, threshold); dir != "LR" {
		t.Errorf("expected LR for count 3 <= threshold 5, got %s", dir)
	}
	if dir := AdaptiveDirection(5, threshold); dir != "LR" {
		t.Errorf("expected LR for count 5 <= threshold 5, got %s", dir)
	}
	if dir := AdaptiveDirection(6, threshold); dir != "TD" {
		t.Errorf("expected TD for count 6 > threshold 5, got %s", dir)
	}
}

func TestGenerateDiagram_BoundedChunkingAndFooter(t *testing.T) {
	// 50 files with chunk size 10 -> would be 5 calls, but budget is 4 calls (D-08, T-04-04-03)
	files := make([]string, 50)
	for i := 0; i < 50; i++ {
		files[i] = "file" + string(rune('A'+i%26)) + string(rune('0'+i/26)) + ".go"
	}

	input := DiagramInput{
		Files:     files,
		Truncated: true,
	}

	diagram, err := GenerateDiagram(context.Background(), nil, input, 5, 50, 100, 32768, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Must carry mermaid block
	if !strings.Contains(diagram, "```mermaid") {
		t.Fatalf("expected mermaid block in diagram, got:\n%s", diagram)
	}

	// 2. Must list unprocessed files (exceeded budget)
	if !strings.Contains(diagram, "Unprocessed files (exceeded diagram budget)") {
		t.Fatalf("expected unprocessed files footer in diagram, got:\n%s", diagram)
	}

	// 3. Must carry partial coverage note
	if !strings.Contains(diagram, "Diagram reflects partial repository changes") {
		t.Fatalf("expected partial coverage note, got:\n%s", diagram)
	}
}
