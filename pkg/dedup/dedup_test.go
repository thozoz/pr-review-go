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
