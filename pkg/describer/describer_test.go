package describer

import (
	"strings"
	"testing"
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
