package reviewer

import (
	"strings"
	"testing"
)

func TestBuildInlineSuggestionsOnlyUsesChangedLines(t *testing.T) {
	diff := "diff --git a/pkg/a.go b/pkg/a.go\nindex 1..2 100644\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -10,2 +10,3 @@ func run() {\n old()\n+new()\n next()\n"
	findings := []Finding{
		{File: "pkg/a.go", Line: 11, Severity: "WARNING", Title: "Fix", Description: "Reason", SuggestedCode: "better()"},
		{File: "pkg/a.go", Line: 12, Severity: "WARNING", Title: "Skip", Description: "Reason", SuggestedCode: "nope()"},
	}

	suggestions := BuildInlineSuggestions(diff, findings)
	if len(suggestions) != 1 {
		t.Fatalf("got %d suggestions, want 1", len(suggestions))
	}
	if suggestions[0].Line != 11 || !strings.Contains(suggestions[0].Body, "```suggestion\nbetter()\n```") {
		t.Fatalf("unexpected suggestion: %#v", suggestions[0])
	}
}

func TestBuildInlineSuggestionsRejectsFenceInCode(t *testing.T) {
	diff := "+++ b/a.go\n@@ -1 +1 @@\n+new()\n"
	findings := []Finding{{File: "a.go", Line: 1, SuggestedCode: "bad\\n```"}}
	if got := BuildInlineSuggestions(diff, findings); len(got) != 0 {
		t.Fatalf("got %d suggestions, want 0", len(got))
	}
}
