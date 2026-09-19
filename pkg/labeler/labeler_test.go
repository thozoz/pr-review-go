package labeler

import (
	"reflect"
	"testing"
)

func TestParseLabelsJSON(t *testing.T) {
	rawJSON := `{"labels": ["Bug fix", "Tests"]}`
	labels, err := parseLabelsJSON(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"Bug fix", "Tests"}
	if !reflect.DeepEqual(labels, expected) {
		t.Errorf("got %v, want %v", labels, expected)
	}

	// Test with markdown codeblock wrapper
	rawMarkdown := "```json\n{\"labels\": [\"enhancement\"]}\n```"
	labelsMD, err := parseLabelsJSON(rawMarkdown)
	if err != nil {
		t.Fatalf("unexpected error with markdown: %v", err)
	}
	expectedMD := []string{"enhancement"}
	if !reflect.DeepEqual(labelsMD, expectedMD) {
		t.Errorf("got %v, want %v", labelsMD, expectedMD)
	}
}

func TestResolveToCanonical(t *testing.T) {
	inputs := []string{"bug_fix", "ENHANCEMENT", "unknown_label", "tests"}
	got := resolveToCanonical(inputs)
	expected := []string{"Bug fix", "Enhancement", "Tests"}

	if !reflect.DeepEqual(got, expected) {
		t.Errorf("got %v, want %v", got, expected)
	}
}
