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

// TestGenerate_Old80KBCutLandedMidHunkYieldsWholeHunkWindowsPlusTruncationFlag constructs a diff
// where the old raw 80000 byte cut would land squarely in the middle of a hunk.
// Proves hunk-atomic windowing keeps the whole hunk before the boundary, drops the exceeding hunk,
// and surfaces the truncation flag without cutting inside any hunk.
func TestGenerate_Old80KBCutLandedMidHunkYieldsWholeHunkWindowsPlusTruncationFlag(t *testing.T) {
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
				{Message: llm.ChatMessage{Role: "assistant", Content: "## Purpose\nSummary.\n\n## Walkthrough\n| File | Change |\n| foo.go | Update |"}},
			},
		})
	}))
	defer llmServer.Close()

	cfg := &config.Config{
		GitHubToken: "token",
		LLMAPIKey:   "key",
		LLMModel:    "model",
		LLMBaseURL:  llmServer.URL,
	}
	d := NewDescriber(cfg)

	// Create diff of length ~80500 where a hunk crosses 80000 bytes
	var sb strings.Builder
	sb.WriteString("diff --git a/pkg/service/service.go b/pkg/service/service.go\n--- a/pkg/service/service.go\n+++ b/pkg/service/service.go\n")

	// Fill with hunks up to byte ~79500
	hunkIndex := 1
	for sb.Len() < 79500 {
		sb.WriteString(fmt.Sprintf("@@ -%d,5 +%d,5 @@ func Worker%d() {\n", hunkIndex*10, hunkIndex*10, hunkIndex))
		for k := 0; k < 5; k++ {
			sb.WriteString(fmt.Sprintf("+	step_%d_%d()\n", hunkIndex, k))
		}
		hunkIndex++
	}

	// Now add a straddling hunk that starts at ~79500 and ends at ~80500 (spanning across the 80000 boundary)
	straddlingHunkHeader := fmt.Sprintf("@@ -%d,50 +%d,50 @@ func StraddlingHunk() {\n", hunkIndex*10, hunkIndex*10)
	sb.WriteString(straddlingHunkHeader)
	for k := 0; k < 50; k++ {
		sb.WriteString(fmt.Sprintf("+	mid_hunk_long_line_%04d_%s\n", k, strings.Repeat("A", 15)))
	}

	rawDiff := sb.String()
	if len(rawDiff) < 80000 {
		t.Fatalf("expected diff > 80000, got %d", len(rawDiff))
	}
	// Verify raw 80000 slice cuts inside the straddling hunk
	if !strings.Contains(rawDiff[:80000], "StraddlingHunk") {
		t.Fatalf("test precondition failed: straddling hunk should start before 80000")
	}

	pr := &github.PRDetails{
		Title:  "Boundary Test PR",
		Author: "bob",
		Body:   "Initial text",
	}

	desc, truncated, err := d.GenerateWithStatus(context.Background(), pr, rawDiff)
	if err != nil {
		t.Fatalf("GenerateWithStatus failed: %v", err)
	}
	if !truncated {
		t.Errorf("expected truncated=true")
	}
	if !strings.Contains(desc, "truncated at a hunk boundary") {
		t.Errorf("expected truncation notice in description output")
	}

	// Verify the diff passed to LLM does NOT contain a sliced straddling hunk:
	// either all lines of StraddlingHunk are present or none are.
	startMarker := "```diff\n"
	startIdx := strings.Index(receivedPrompt, startMarker)
	endIdx := strings.LastIndex(receivedPrompt, "```")
	diffContent := receivedPrompt[startIdx+len(startMarker) : endIdx]

	if strings.Contains(diffContent, "StraddlingHunk") {
		// If included, every single one of its 50 lines must be present intact
		for k := 0; k < 50; k++ {
			expectedLine := fmt.Sprintf("+	mid_hunk_long_line_%04d_", k)
			if !strings.Contains(diffContent, expectedLine) {
				t.Fatalf("hunk was sliced inside body! Missing line %d", k)
			}
		}
	}
}

func TestRunAndUpdate_IdempotencyMarkerUntouched(t *testing.T) {
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pulls/42") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42,
				"title":  "Existing PR",
				"body":   "Author notes\n\n" + marker + "\n## Purpose\nAlready generated",
				"user":   map[string]any{"login": "charlie"},
			})
			return
		}
		// If LLM or update endpoint is called, fail the test
		t.Errorf("unexpected HTTP call to %s %s", r.Method, r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer ghServer.Close()

	cfg := &config.Config{
		GitHubToken: "gh-token",
		LLMAPIKey:   "llm-key",
		LLMModel:    "llm-model",
		LLMBaseURL:  "http://unused.example.com",
	}
	d := NewDescriber(cfg)
	ghCl, err := github.NewTestClient(ghServer.URL)
	if err != nil {
		t.Fatalf("failed to create test github client: %v", err)
	}
	d.gh = ghCl

	updated, err := d.RunAndUpdate(context.Background(), "owner", "repo", 42)
	if err != nil {
		t.Fatalf("RunAndUpdate failed: %v", err)
	}
	if updated {
		t.Fatalf("expected updated=false when marker already present in PR body (idempotency)")
	}
}
