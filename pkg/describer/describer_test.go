package describer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/llm"
)

func TestAppendDescriptionPreservesAuthorBody(t *testing.T) {
	body := appendDescription("Author context", "## Purpose\nGenerated")
	if !strings.HasPrefix(body, "Author context\n\n---") || !strings.Contains(body, marker) {
		t.Fatalf("author body was not preserved: %q", body)
	}
}

func TestAppendDescriptionFillsEmptyBody(t *testing.T) {
	body := appendDescription("  ", "## Purpose\nGenerated")
	if body != marker+"\n## Purpose\nGenerated" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestGenerate_TruncatedDiffSurfacesFlagAndNotice(t *testing.T) {
	var receivedPrompt string
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 1 {
			receivedPrompt = req.Messages[1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llm.ChatResponse{
			Choices: []llm.ChatChoice{
				{Message: llm.ChatMessage{Role: "assistant", Content: "## Purpose\nAdds feature.\n\n## Walkthrough\n| File | Change |\n| foo.go | Added feature |"}},
			},
		})
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		GitHubToken: "test-token",
		LLMAPIKey:   "test-key",
		LLMModel:    "test-model",
		LLMBaseURL:  llmServer.URL,
	}
	d := NewDescriber(cfg)

	// Build a diff exceeding 80000 bytes with multiple hunks
	var sb strings.Builder
	sb.WriteString("diff --git a/pkg/foo/foo.go b/pkg/foo/foo.go\n--- a/pkg/foo/foo.go\n+++ b/pkg/foo/foo.go\n")
	for i := 0; i < 200; i++ {
		sb.WriteString(fmt.Sprintf("@@ -%d,10 +%d,10 @@ func F%d() {\n", i*10+1, i*10+1, i))
		for j := 0; j < 10; j++ {
			sb.WriteString(fmt.Sprintf("+	// line %d in hunk %d - some extra padding text to exceed limits quickly\n", j, i))
		}
	}
	largeDiff := sb.String()
	if len(largeDiff) <= 80000 {
		t.Fatalf("test diff must exceed 80000 bytes, got %d", len(largeDiff))
	}

	pr := &github.PRDetails{
		Title:  "Big PR",
		Author: "alice",
		Body:   "Author notes",
	}

	desc, truncated, err := d.GenerateWithStatus(context.Background(), pr, largeDiff)
	if err != nil {
		t.Fatalf("GenerateWithStatus failed: %v", err)
	}

	if !truncated {
		t.Fatalf("expected truncated=true for large diff")
	}

	if !strings.Contains(desc, "truncated at a hunk boundary") {
		t.Fatalf("expected truncation notice in output, got: %s", desc)
	}

	if !strings.Contains(receivedPrompt, "...[diff truncated at hunk boundary]...") {
		t.Fatalf("expected diff truncated marker in prompt, got prompt: %s", receivedPrompt)
	}

	// Verify no hunk in the prompt diff was sliced mid-line or mid-hunk
	startMarker := "```diff\n"
	startIdx := strings.Index(receivedPrompt, startMarker)
	if startIdx < 0 {
		t.Fatalf("missing diff start marker in prompt")
	}
	endIdx := strings.LastIndex(receivedPrompt, "```")
	if endIdx <= startIdx+len(startMarker) {
		t.Fatalf("invalid diff section bounds in prompt")
	}
	diffSection := receivedPrompt[startIdx+len(startMarker) : endIdx]
	lines := strings.Split(diffSection, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") && !strings.HasSuffix(l, "quickly") {
			t.Errorf("found truncated/cut line inside hunk: %q", l)
		}
	}
}
