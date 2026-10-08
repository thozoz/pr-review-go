package reviewer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thozoz/pr-review-go/pkg/diff"
)

func TestReport_FullCoverage_ZeroFindings(t *testing.T) {
	rawDiff := `diff --git a/main.go b/main.go
index 123..456 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+func Hello() {}
`
	inv := diff.Parse(rawDiff)
	ledger := diff.NewCoverageLedger(inv)
	ledger.MarkFileExamined("main.go")

	report := &ReviewReport{
		PRTitle: "Test Full Coverage",
		Score:   95,
		Summary: "All files reviewed cleanly.",
		Ledger:  ledger,
	}

	markdown := FormatReportMarkdown(report)

	if !strings.Contains(markdown, "Looks ready to merge!") {
		t.Fatalf("expected full coverage with zero findings to say 'Looks ready to merge!', got:\n%s", markdown)
	}
	if strings.Contains(markdown, "Partial Review Warning") {
		t.Fatalf("expected no partial review warning on full coverage, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "Status:** Full") {
		t.Fatalf("expected coverage status Full, got:\n%s", markdown)
	}
}

func TestReport_PartialCoverage_ZeroFindings(t *testing.T) {
	rawDiff := `diff --git a/a.go b/a.go
index 123..456 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,4 @@
+func A() {}
diff --git a/b.go b/b.go
index 123..456 100644
--- a/b.go
+++ b/b.go
@@ -1,3 +1,4 @@
+func B() {}
`
	inv := diff.Parse(rawDiff)
	ledger := diff.NewCoverageLedger(inv)
	// Examine a.go, leave b.go skipped
	ledger.MarkFileExamined("a.go")

	report := &ReviewReport{
		PRTitle: "Test Partial Coverage",
		Score:   85,
		Summary: "Reviewed part of the PR.",
		Ledger:  ledger,
	}

	markdown := FormatReportMarkdown(report)

	// D-06: No partial fixture contains merge-ready phrasing in any casing
	lower := strings.ToLower(markdown)
	if strings.Contains(lower, "ready to merge") {
		t.Fatalf("partial review must NOT contain 'ready to merge' in any casing, got:\n%s", markdown)
	}

	// Warning must be present and appear before findings
	warningIdx := strings.Index(markdown, "Partial Review Warning")
	findingsIdx := strings.Index(markdown, "### 🎯 Findings")
	if warningIdx == -1 {
		t.Fatalf("expected Partial Review Warning, got:\n%s", markdown)
	}
	if findingsIdx == -1 {
		t.Fatalf("expected Findings section, got:\n%s", markdown)
	}
	if warningIdx > findingsIdx {
		t.Fatalf("expected Partial Review Warning to precede Findings, warningIdx=%d findingsIdx=%d", warningIdx, findingsIdx)
	}

	// Unexamined changes must be explicitly identified
	if !strings.Contains(markdown, "b.go#H1") {
		t.Fatalf("expected skipped hunk b.go#H1 in markdown, got:\n%s", markdown)
	}
}

func TestReport_OmissionRecords_RenderedWithReasons(t *testing.T) {
	inv := &diff.ChangeInventory{
		Files: []*diff.FileDiff{
			{
				Path:     "image.png",
				IsBinary: true,
			},
			{
				Path:       "script.sh",
				IsModeOnly: true,
			},
		},
		Omissions: []*diff.Omission{
			{
				File:   "big.json",
				Reason: "exceeded diff size limit (DIFF_MAX_BYTES=2097152)",
			},
		},
	}
	ledger := diff.NewCoverageLedger(inv)

	report := &ReviewReport{
		PRTitle: "Omission Test",
		Score:   70,
		Summary: "PR with non-code changes.",
		Ledger:  ledger,
	}

	markdown := FormatReportMarkdown(report)

	if !strings.Contains(markdown, "Omissions & Unsupported Changes") {
		t.Fatalf("expected Omissions section, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "binary file") {
		t.Fatalf("expected 'binary file' reason, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "mode change only") {
		t.Fatalf("expected 'mode change only' reason, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "exceeded diff size limit") {
		t.Fatalf("expected 'exceeded diff size limit' reason, got:\n%s", markdown)
	}
}

func TestReport_BoundedList_500SkippedHunks(t *testing.T) {
	inv := &diff.ChangeInventory{
		Files: []*diff.FileDiff{},
	}
	for i := 1; i <= 500; i++ {
		inv.Files = append(inv.Files, &diff.FileDiff{
			Path: fmt.Sprintf("file_%03d.go", i),
			Hunks: []*diff.Hunk{
				{
					ID:   fmt.Sprintf("file_%03d.go#H1", i),
					File: fmt.Sprintf("file_%03d.go", i),
				},
			},
		})
	}

	ledger := diff.NewCoverageLedger(inv)
	// Examine first 5
	for i := 1; i <= 5; i++ {
		ledger.MarkFileExamined(fmt.Sprintf("file_%03d.go", i))
	}

	report := &ReviewReport{
		PRTitle: "Large PR",
		Score:   50,
		Summary: "Very large PR with 500 hunks.",
		Ledger:  ledger,
	}

	markdown := FormatReportMarkdown(report)

	// Total skipped must reconcile to ledger count (495)
	if !strings.Contains(markdown, "495 skipped") {
		t.Fatalf("expected summary line to state 495 skipped, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "495 total skipped") {
		t.Fatalf("expected truncation note to reconcile to 495 total skipped, got:\n%s", markdown)
	}
	// Truncation note must be present
	if !strings.Contains(markdown, "more skipped hunks") {
		t.Fatalf("expected truncation note for large skipped list, got:\n%s", markdown)
	}

	// Negative assertion: no merge-ready phrasing
	if strings.Contains(strings.ToLower(markdown), "ready to merge") {
		t.Fatalf("partial report must not contain ready to merge, got:\n%s", markdown)
	}
}

func TestReport_DedupInterplay_PartialReview(t *testing.T) {
	rawDiff := `diff --git a/a.go b/a.go
index 123..456 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,4 @@
+func A() {}
`
	inv := diff.Parse(rawDiff)
	ledger := diff.NewCoverageLedger(inv)
	// Leave a.go skipped -> partial review

	report := &ReviewReport{
		PRTitle:           "Dedup Partial",
		Score:             90,
		Summary:           "Dedup test.",
		DeduplicatedCount: 3,
		Findings:          []Finding{}, // All deduplicated
		Ledger:            ledger,
	}

	markdown := FormatReportMarkdown(report)

	// Dedup message present
	if !strings.Contains(markdown, "All 3 detected issues were already reported previously and have been deduplicated") {
		t.Fatalf("expected dedup message, got:\n%s", markdown)
	}
	// Must carry partial note in dedup message
	if !strings.Contains(markdown, "unexamined changes have not been verified") {
		t.Fatalf("expected unexamined changes note with dedup, got:\n%s", markdown)
	}
	// Negative assertion: no merge-ready phrasing
	if strings.Contains(strings.ToLower(markdown), "ready to merge") {
		t.Fatalf("must not claim ready to merge on dedup partial, got:\n%s", markdown)
	}
}

func TestReport_FingerprintEmission(t *testing.T) {
	report := &ReviewReport{
		PRTitle: "Findings Test",
		Score:   60,
		Summary: "Issues found.",
		Findings: []Finding{
			{
				File:        "pkg/auth/token.go",
				Line:        42,
				Severity:    "CRITICAL",
				Title:       "Goroutine leak",
				Description: "Missing cancel.",
				Suggestion:  "Add context check.",
			},
		},
	}

	markdown := FormatReportMarkdown(report)

	if !strings.Contains(markdown, "<!-- pr-review-go:fingerprint=") {
		t.Fatalf("expected fingerprint comment in findings, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "[CRITICAL] Goroutine leak (`pkg/auth/token.go:42`)") {
		t.Fatalf("expected finding header, got:\n%s", markdown)
	}
}

func TestReport_LegacyReportWithoutLedger(t *testing.T) {
	report := &ReviewReport{
		PRTitle: "Legacy PR",
		Score:   100,
		Summary: "Legacy review path.",
	}

	markdown := FormatReportMarkdown(report)

	if !strings.Contains(markdown, "Looks ready to merge!") {
		t.Fatalf("expected legacy review with no ledger and zero findings to say 'Looks ready to merge!', got:\n%s", markdown)
	}
	if strings.Contains(markdown, "Review Coverage") {
		t.Fatalf("expected no Review Coverage section when ledger is nil, got:\n%s", markdown)
	}
}

func TestReport_FullCoverage_WithFindings_NoApproval(t *testing.T) {
	rawDiff := `diff --git a/main.go b/main.go
index 123..456 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+func Hello() {}
`
	inv := diff.Parse(rawDiff)
	ledger := diff.NewCoverageLedger(inv)
	ledger.MarkFileExamined("main.go")

	report := &ReviewReport{
		PRTitle: "Test Full Coverage With Findings",
		Score:   60,
		Summary: "One issue found in fully examined changes.",
		Findings: []Finding{
			{
				File:        "main.go",
				Line:        2,
				Severity:    "WARNING",
				Title:       "Missing doc comment",
				Description: "Exported function lacks a doc comment.",
				Suggestion:  "Add a doc comment.",
			},
		},
		Ledger: ledger,
	}

	markdown := FormatReportMarkdown(report)

	// DIFF-01 / UAT-5-second-half: full coverage WITH findings must NOT approve.
	lower := strings.ToLower(markdown)
	if strings.Contains(lower, "ready to merge") {
		t.Fatalf("full coverage with findings must NOT contain 'ready to merge' in any casing, got:\n%s", markdown)
	}

	// The finding itself must be rendered, not silently dropped.
	if !strings.Contains(markdown, "Missing doc comment") {
		t.Fatalf("expected finding title rendered, got:\n%s", markdown)
	}
	if !strings.Contains(markdown, "[WARNING] Missing doc comment (`main.go:2`)") {
		t.Fatalf("expected finding header rendered, got:\n%s", markdown)
	}

	// Coverage state must read Full with no partial warning.
	if !strings.Contains(markdown, "Status:** Full") {
		t.Fatalf("expected coverage status Full, got:\n%s", markdown)
	}
	if strings.Contains(markdown, "Partial Review Warning") {
		t.Fatalf("expected no partial review warning on full coverage, got:\n%s", markdown)
	}
}
