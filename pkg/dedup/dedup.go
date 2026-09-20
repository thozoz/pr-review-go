package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	FingerprintPrefix = "<!-- pr-review-go:fingerprint="
	FingerprintSuffix = " -->"
)

var fingerprintRegex = regexp.MustCompile(`<!--\s*pr-review-go:fingerprint=([a-f0-9]{64})\s*-->`)

// ComputeFingerprint creates a deterministic SHA-256 hash for a specific finding.
// It combines normalized file path, approximate line, finding title, and severity.
func ComputeFingerprint(file string, line int, title, severity string) string {
	cleanFile := strings.TrimSpace(strings.ToLower(file))
	cleanTitle := strings.TrimSpace(strings.ToLower(title))
	cleanSev := strings.TrimSpace(strings.ToUpper(severity))

	// Format: "file:line:title:severity"
	raw := fmt.Sprintf("%s:%d:%s:%s", cleanFile, line, cleanTitle, cleanSev)
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

// EmbedMarker attaches the hidden HTML fingerprint tag to a markdown comment body.
func EmbedMarker(markdown string, fingerprint string) string {
	marker := fmt.Sprintf("%s%s%s", FingerprintPrefix, fingerprint, FingerprintSuffix)
	return fmt.Sprintf("%s\n\n%s", strings.TrimSpace(markdown), marker)
}

// ExtractFingerprints scans a list of comment bodies and returns a set of existing fingerprints.
func ExtractFingerprints(comments []string) map[string]bool {
	existing := make(map[string]bool)
	for _, c := range comments {
		matches := fingerprintRegex.FindAllStringSubmatch(c, -1)
		for _, m := range matches {
			if len(m) >= 2 {
				existing[m[1]] = true
			}
		}
	}
	return existing
}
