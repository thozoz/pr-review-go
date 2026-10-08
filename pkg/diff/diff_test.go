package diff

import (
	"strings"
	"testing"
)

func sampleUnifiedDiff() string {
	return `diff --git a/pkg/foo/foo.go b/pkg/foo/foo.go
index 1111111..2222222 100644
--- a/pkg/foo/foo.go
+++ b/pkg/foo/foo.go
@@ -10,6 +10,8 @@ package foo
 func Alpha() {
+	// added line 11
+	// added line 12
 	doSomething()
 }
@@ -30,5 +32,6 @@ func Beta() {
 	oldWork()
+	newWork()
 }
diff --git a/pkg/bar/bar.go b/pkg/bar/bar.go
index 3333333..4444444 100644
--- a/pkg/bar/bar.go
+++ b/pkg/bar/bar.go
@@ -1,4 +1,5 @@
 package bar
+import "fmt"
 func Bar() {}
`
}

func TestParseBasicDiffAndChangedPRLines(t *testing.T) {
	raw := sampleUnifiedDiff()
	inv := Parse(raw)

	if len(inv.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(inv.Files))
	}

	fooFile := inv.FindFile("pkg/foo/foo.go")
	if fooFile == nil {
		t.Fatalf("expected pkg/foo/foo.go in inventory")
	}
	if len(fooFile.Hunks) != 2 {
		t.Fatalf("expected 2 hunks in pkg/foo/foo.go, got %d", len(fooFile.Hunks))
	}

	h1 := fooFile.Hunks[0]
	if h1.NewStart != 10 || h1.NewCount != 8 {
		t.Errorf("unexpected h1 ranges: newStart=%d, newCount=%d", h1.NewStart, h1.NewCount)
	}
	if !h1.AddedLines[11] || !h1.AddedLines[12] {
		t.Errorf("expected added lines 11 and 12 in h1, got: %v", h1.AddedLines)
	}

	changed := inv.ChangedPRLines()
	if !changed["pkg/foo/foo.go"][11] || !changed["pkg/foo/foo.go"][12] {
		t.Errorf("expected ChangedPRLines to contain added lines 11 and 12")
	}
	if changed["pkg/foo/foo.go"][10] {
		t.Errorf("line 10 is context, should not be in ChangedPRLines")
	}
}

// Tracer: D-05: diff windowing never cuts inside a hunk. Mid-diff hunk survives windowing intact.
func TestHunkSurvivesWindowingIntact(t *testing.T) {
	raw := sampleUnifiedDiff()
	inv := Parse(raw)

	// Choose a byte budget that holds file header + hunk 1, but cannot hold hunk 2.
	fooHunk1 := inv.Files[0].Hunks[0]
	fooHunk2 := inv.Files[0].Hunks[1]
	hunk1Text := fooHunk1.Header + "\n" + strings.Join(fooHunk1.Lines, "\n") + "\n"
	fileHeaderLen := len("diff --git a/pkg/foo/foo.go b/pkg/foo/foo.go\n--- a/pkg/foo/foo.go\n+++ b/pkg/foo/foo.go\n")
	budget := fileHeaderLen + len(hunk1Text) + 5 // not enough for hunk 2 (which is > 30 bytes)

	windows := inv.Windows(budget)
	if len(windows) < 2 {
		t.Fatalf("expected at least 2 windows with budget %d, got %d", budget, len(windows))
	}

	// Verify Window 0 contains whole hunk 1 and does not cut into hunk 2
	w0 := windows[0]
	if len(w0.Hunks) != 1 {
		t.Fatalf("expected exactly 1 hunk in window 0, got %d", len(w0.Hunks))
	}
	if w0.Hunks[0].ID != fooHunk1.ID {
		t.Errorf("expected hunk %s in window 0, got %s", fooHunk1.ID, w0.Hunks[0].ID)
	}
	// Content must contain complete hunk body, never sliced inside @@ or body
	if !strings.Contains(w0.Content, fooHunk1.Header) {
		t.Errorf("window 0 missing hunk header")
	}
	if !strings.Contains(w0.Content, "doSomething()") {
		t.Errorf("window 0 cut hunk 1 body prematurely")
	}

	// Window 1 contains hunk 2 intact
	w1 := windows[1]
	if len(w1.Hunks) == 0 {
		t.Fatalf("expected hunks in window 1")
	}
	if w1.Hunks[0].ID != fooHunk2.ID {
		t.Errorf("expected hunk %s in window 1, got %s", fooHunk2.ID, w1.Hunks[0].ID)
	}
	if !strings.Contains(w1.Content, fooHunk2.Header) || !strings.Contains(w1.Content, "newWork()") {
		t.Errorf("window 1 cut hunk 2 body prematurely")
	}
}

// Tracer: Oversized input produces omission records with reason naming the exact bound hit.
func TestOversizedInputProducesOmissionRecords(t *testing.T) {
	// 1. Oversized raw diff exceeding MaxBytes
	hugeDiff := strings.Repeat("diff --git a/huge.go b/huge.go\n--- a/huge.go\n+++ b/huge.go\n@@ -1,1 +1,2 @@\n+line\n", 100)
	opts := ParseOptions{
		MaxBytes: 500, // very small cap
		MaxFiles: 2000,
		MaxHunks: 20000,
	}
	inv := Parse(hugeDiff, opts)
	if !inv.Truncated {
		t.Errorf("expected inventory Truncated=true on oversized input")
	}
	if len(inv.Omissions) == 0 {
		t.Fatalf("expected omission records for oversized diff")
	}
	foundByteOmission := false
	for _, om := range inv.Omissions {
		if strings.Contains(om.Reason, "DIFF_MAX_BYTES") {
			foundByteOmission = true
		}
	}
	if !foundByteOmission {
		t.Errorf("expected omission reason to mention DIFF_MAX_BYTES, got: %v", inv.Omissions[0].Reason)
	}

	// 2. Single hunk exceeding window byte budget becomes an omission record in windowing
	singleHunkDiff := `diff --git a/big.go b/big.go
--- a/big.go
+++ b/big.go
@@ -1,5 +1,5 @@
-old line 1
-old line 2
+new line 1
+new line 2
`
	bigInv := Parse(singleHunkDiff)
	// Window with tiny budget smaller than the hunk
	tinyWindow := bigInv.Windows(20)
	if len(tinyWindow) == 0 {
		t.Fatalf("expected at least one window object")
	}
	w := tinyWindow[0]
	if !w.Truncated {
		t.Errorf("expected window to be marked Truncated")
	}
	if len(w.Omissions) == 0 {
		t.Fatalf("expected omission record for hunk exceeding window budget")
	}
	if !strings.Contains(w.Omissions[0].Reason, "exceeds window byte budget") {
		t.Errorf("unexpected omission reason: %s", w.Omissions[0].Reason)
	}
}

func TestCoverageLedgerStartsSkippedAndTracksExamined(t *testing.T) {
	raw := sampleUnifiedDiff()
	inv := Parse(raw)

	ledger := NewCoverageLedger(inv)

	// Contract: CoverageLedger starts every hunk as skipped with reason "not examined"
	if ledger.TotalHunks() != 3 {
		t.Fatalf("expected 3 hunks tracked in ledger, got %d", ledger.TotalHunks())
	}
	if ledger.ExaminedCount() != 0 {
		t.Errorf("expected 0 examined at start, got %d", ledger.ExaminedCount())
	}
	if ledger.SkippedCount() != 3 {
		t.Errorf("expected 3 skipped at start, got %d", ledger.SkippedCount())
	}
	if ledger.IsComplete() {
		t.Errorf("expected IsComplete() = false at start")
	}

	for _, entry := range ledger.Entries() {
		if entry.Status != "skipped" || entry.Reason != "not examined" {
			t.Errorf("entry %s expected skipped with 'not examined', got %s (%s)", entry.HunkID, entry.Status, entry.Reason)
		}
	}

	// Mark one examined
	hunk1ID := "pkg/foo/foo.go#H1"
	ledger.MarkExamined(hunk1ID)
	if !ledger.IsExamined(hunk1ID) {
		t.Errorf("expected hunk1 to be examined")
	}
	if ledger.ExaminedCount() != 1 || ledger.SkippedCount() != 2 {
		t.Errorf("expected 1 examined and 2 skipped, got %d and %d", ledger.ExaminedCount(), ledger.SkippedCount())
	}

	// Mark entire file examined
	ledger.MarkFileExamined("pkg/foo/foo.go")
	if ledger.ExaminedCount() != 2 || ledger.SkippedCount() != 1 {
		t.Errorf("expected 2 examined (both foo hunks) and 1 skipped (bar hunk)")
	}

	// Mark remaining examined
	ledger.MarkExamined("pkg/bar/bar.go#H1")
	if !ledger.IsComplete() {
		t.Errorf("expected IsComplete() = true after all hunks examined")
	}
}
