package dedup

import (
	"testing"
)

func TestComputeFingerprint(t *testing.T) {
	fp1 := ComputeFingerprint("pkg/auth/token.go", 42, "Nil pointer dereference", "CRITICAL")
	fp2 := ComputeFingerprint("PKG/AUTH/TOKEN.GO", 42, "nil pointer dereference", "critical")
	fp3 := ComputeFingerprint("pkg/auth/token.go", 43, "Nil pointer dereference", "CRITICAL")

	if fp1 == "" {
		t.Errorf("expected non-empty fingerprint")
	}

	// Normalization test (case insensitive file, title, severity)
	if fp1 != fp2 {
		t.Errorf("expected fp1 == fp2 after normalization, got %s != %s", fp1, fp2)
	}

	// Different line test
	if fp1 == fp3 {
		t.Errorf("expected fp1 != fp3 for different line, got %s == %s", fp1, fp3)
	}
}

func TestEmbedAndExtractFingerprints(t *testing.T) {
	fp := ComputeFingerprint("main.go", 10, "Resource leak", "WARNING")

	comment := "Here is a code review comment finding."
	embedded := EmbedMarker(comment, fp)

	comments := []string{
		"Regular comment with no fingerprint",
		embedded,
		"Another comment <!-- pr-review-go:fingerprint=1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef -->",
	}

	extracted := ExtractFingerprints(comments)

	if !extracted[fp] {
		t.Errorf("expected fingerprint %s to be extracted", fp)
	}
	if !extracted["1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"] {
		t.Errorf("expected hardcoded fingerprint to be extracted")
	}
	if len(extracted) != 2 {
		t.Errorf("expected 2 extracted fingerprints, got %d", len(extracted))
	}
}

// TestExactSuppressionPins pins D-10: fingerprint suppression is exact.
// Identical findings suppress; line-shifted, retitled, and severity-changed
// variants must NOT suppress (a shifted finding duplicates rather than hides).
// Case/whitespace normalization still applies.
func TestExactSuppressionPins(t *testing.T) {
	base := ComputeFingerprint("pkg/auth/token.go", 42, "Nil pointer dereference", "CRITICAL")
	prior := ExtractFingerprints([]string{EmbedMarker("finding body", base)})

	suppressed := func(fp string) bool { return prior[fp] }

	tests := []struct {
		name     string
		fp       string
		suppress bool
	}{
		{
			name:     "identical suppressed",
			fp:       ComputeFingerprint("pkg/auth/token.go", 42, "Nil pointer dereference", "CRITICAL"),
			suppress: true,
		},
		{
			name:     "line-shifted same-title NOT suppressed",
			fp:       ComputeFingerprint("pkg/auth/token.go", 43, "Nil pointer dereference", "CRITICAL"),
			suppress: false,
		},
		{
			name:     "retitled NOT suppressed",
			fp:       ComputeFingerprint("pkg/auth/token.go", 42, "Nil pointer dereference in handler", "CRITICAL"),
			suppress: false,
		},
		{
			name:     "severity change NOT suppressed",
			fp:       ComputeFingerprint("pkg/auth/token.go", 42, "Nil pointer dereference", "WARNING"),
			suppress: false,
		},
		{
			name:     "case normalization still suppresses",
			fp:       ComputeFingerprint("PKG/AUTH/TOKEN.GO", 42, "nil pointer dereference", "critical"),
			suppress: true,
		},
		{
			name:     "whitespace normalization still suppresses",
			fp:       ComputeFingerprint("  pkg/auth/token.go  ", 42, "  Nil pointer dereference ", "  critical "),
			suppress: true,
		},
		{
			name:     "different file NOT suppressed",
			fp:       ComputeFingerprint("pkg/auth/other.go", 42, "Nil pointer dereference", "CRITICAL"),
			suppress: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := suppressed(tc.fp); got != tc.suppress {
				t.Errorf("suppression mismatch for %q: got %v, want %v", tc.name, got, tc.suppress)
			}
		})
	}
}
