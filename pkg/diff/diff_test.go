package diff

import (
	"fmt"
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

func TestAdversarialDiffCorpus(t *testing.T) {
	tests := []struct {
		name      string
		rawDiff   string
		assertInv func(t *testing.T, inv *ChangeInventory)
	}{
		{
			name: "rename with similarity content",
			rawDiff: `diff --git a/old_name.go b/new_name.go
similarity index 92%
rename from old_name.go
rename to new_name.go
--- a/old_name.go
+++ b/new_name.go
@@ -1,3 +1,4 @@
 package old
+package new
 func Main() {}
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				f := inv.Files[0]
				if !f.IsRename {
					t.Errorf("expected IsRename=true")
				}
				if f.Similarity != 92 {
					t.Errorf("expected Similarity=92, got %d", f.Similarity)
				}
				if f.OldPath != "old_name.go" || f.Path != "new_name.go" {
					t.Errorf("unexpected paths: old=%q, new=%q", f.OldPath, f.Path)
				}
				if len(f.Hunks) != 1 {
					t.Errorf("expected 1 hunk, got %d", len(f.Hunks))
				}
			},
		},
		{
			name: "binary patch markers",
			rawDiff: `diff --git a/assets/image.png b/assets/image.png
new file mode 100644
index 0000000..abcdef1
Binary files /dev/null and b/assets/image.png differ
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				f := inv.Files[0]
				if !f.IsBinary {
					t.Errorf("expected IsBinary=true")
				}
				windows := inv.Windows(50000)
				if len(windows) == 0 {
					t.Fatalf("expected at least 1 window")
				}
				foundBinaryOmission := false
				for _, om := range windows[0].Omissions {
					if om.Reason == "binary file" {
						foundBinaryOmission = true
					}
				}
				if !foundBinaryOmission {
					t.Errorf("expected window omission for binary file")
				}
			},
		},
		{
			name: "git binary patch header",
			rawDiff: `diff --git a/assets/blob.bin b/assets/blob.bin
index 1111111..2222222 100644
GIT binary patch
literal 1234
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				if !inv.Files[0].IsBinary {
					t.Errorf("expected IsBinary=true for GIT binary patch")
				}
			},
		},
		{
			name: "mode-only change",
			rawDiff: `diff --git a/deploy.sh b/deploy.sh
old mode 100644
new mode 100755
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				f := inv.Files[0]
				if !f.IsModeOnly {
					t.Errorf("expected IsModeOnly=true")
				}
				if f.OldMode != "100644" || f.NewMode != "100755" {
					t.Errorf("unexpected modes: old=%q, new=%q", f.OldMode, f.NewMode)
				}
				if len(f.Hunks) != 0 {
					t.Errorf("expected 0 hunks for mode-only change, got %d", len(f.Hunks))
				}
				windows := inv.Windows(50000)
				foundModeOmission := false
				for _, om := range windows[0].Omissions {
					if om.Reason == "mode change only" {
						foundModeOmission = true
					}
				}
				if !foundModeOmission {
					t.Errorf("expected window omission for mode-only change")
				}
			},
		},
		{
			name:    "empty diff",
			rawDiff: "   \n\n  \t  \n",
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 0 {
					t.Errorf("expected 0 files, got %d", len(inv.Files))
				}
				if inv.TotalHunks != 0 {
					t.Errorf("expected 0 hunks, got %d", inv.TotalHunks)
				}
				if len(inv.Omissions) != 0 {
					t.Errorf("expected 0 omissions, got %d", len(inv.Omissions))
				}
			},
		},
		{
			name: "diff with only deletions",
			rawDiff: `diff --git a/pkg/dep/dep.go b/pkg/dep/dep.go
--- a/pkg/dep/dep.go
+++ b/pkg/dep/dep.go
@@ -1,5 +1,2 @@
-line 1
-line 2
-line 3
 context 4
 context 5
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				f := inv.Files[0]
				if len(f.Hunks) != 1 {
					t.Fatalf("expected 1 hunk, got %d", len(f.Hunks))
				}
				h := f.Hunks[0]
				if len(h.AddedLines) != 0 {
					t.Errorf("expected 0 added lines for deletions-only hunk, got %v", h.AddedLines)
				}
				changed := inv.ChangedPRLines()
				if len(changed["pkg/dep/dep.go"]) != 0 {
					t.Errorf("expected no added lines in ChangedPRLines for deleted lines")
				}
			},
		},
		{
			name: "hunk header with zero new-side length",
			rawDiff: `diff --git a/pkg/zero/zero.go b/pkg/zero/zero.go
--- a/pkg/zero/zero.go
+++ b/pkg/zero/zero.go
@@ -10,5 +15,0 @@
-line 10
-line 11
-line 12
-line 13
-line 14
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Files) != 1 {
					t.Fatalf("expected 1 file, got %d", len(inv.Files))
				}
				f := inv.Files[0]
				if len(f.Hunks) != 1 {
					t.Fatalf("expected 1 hunk, got %d", len(f.Hunks))
				}
				h := f.Hunks[0]
				if h.NewStart != 15 || h.NewCount != 0 {
					t.Errorf("expected newStart=15, newCount=0, got start=%d, count=%d", h.NewStart, h.NewCount)
				}
				if h.OldStart != 10 || h.OldCount != 5 {
					t.Errorf("expected oldStart=10, oldCount=5, got start=%d, count=%d", h.OldStart, h.OldCount)
				}
			},
		},
		{
			name: "malformed hunk header recorded never panics",
			rawDiff: `diff --git a/pkg/bad/bad.go b/pkg/bad/bad.go
--- a/pkg/bad/bad.go
+++ b/pkg/bad/bad.go
@@ invalid hunk header @@
+some line
@@ -not-a-number,5 +10,5 @@
+another line
`,
			assertInv: func(t *testing.T, inv *ChangeInventory) {
				if len(inv.Omissions) < 2 {
					t.Fatalf("expected at least 2 omissions for malformed hunk headers, got %d", len(inv.Omissions))
				}
				for _, om := range inv.Omissions {
					if !strings.Contains(om.Reason, "malformed hunk header") {
						t.Errorf("expected omission reason to mention malformed hunk header, got: %s", om.Reason)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parse panicked on corpus input %q: %v", tt.name, r)
				}
			}()
			inv := Parse(tt.rawDiff)
			tt.assertInv(t, inv)
		})
	}
}

// TestAdversarial_3MiBSingleFileInput verifies that a 3MiB single-file input parses
// without panic, stays under memory bounds, and records omission naming DIFF_MAX_BYTES.
func TestAdversarial_3MiBSingleFileInput(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("diff --git a/pkg/huge/huge.go b/pkg/huge/huge.go\n--- a/pkg/huge/huge.go\n+++ b/pkg/huge/huge.go\n")

	// Target 3 MiB: 3 * 1024 * 1024 = 3,145,728 bytes
	linePayload := "+// " + strings.Repeat("x", 200) + "\n"
	hunkHeader := "@@ -1,1000 +1,1000 @@\n"
	sb.WriteString(hunkHeader)

	for sb.Len() < 3*1024*1024 {
		sb.WriteString(linePayload)
	}

	raw := sb.String()
	if len(raw) < 3*1024*1024 {
		t.Fatalf("expected diff size >= 3MiB, got %d", len(raw))
	}

	inv := Parse(raw) // Uses default DIFF_MAX_BYTES = 2 MiB
	if !inv.Truncated {
		t.Errorf("expected inventory Truncated=true on 3MiB input")
	}

	foundBoundReason := false
	for _, om := range inv.Omissions {
		if strings.Contains(om.Reason, "DIFF_MAX_BYTES") {
			foundBoundReason = true
			break
		}
	}
	if !foundBoundReason {
		t.Errorf("expected omission naming DIFF_MAX_BYTES, got omissions: %+v", inv.Omissions)
	}

	// Verify hunks parsed within the 2MiB cut remain intact
	if len(inv.Files) == 0 {
		t.Fatalf("expected at least 1 file parsed within budget")
	}
	f := inv.Files[0]
	if len(f.Hunks) == 0 {
		t.Fatalf("expected hunks parsed within budget")
	}
	for _, h := range f.Hunks {
		if !strings.HasPrefix(h.Header, "@@") {
			t.Errorf("corrupted hunk header: %s", h.Header)
		}
	}
}

// TestAdversarial_2500FileInput verifies that an input with 2500 files hits the DIFF_MAX_FILES cap (2000),
// terminates parsing cleanly, and records the exact bound hit.
func TestAdversarial_2500FileInput(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString(fmt.Sprintf("diff --git a/f%d.go b/f%d.go\n--- a/f%d.go\n+++ b/f%d.go\n@@ -1,1 +1,2 @@\n+line%d\n", i, i, i, i, i))
	}

	raw := sb.String()
	// Set MaxBytes generous so only the file limit (DIFF_MAX_FILES=2000) triggers
	opts := ParseOptions{
		MaxBytes: 20 * 1024 * 1024,
		MaxFiles: 2000,
		MaxHunks: 50000,
	}

	inv := Parse(raw, opts)
	if len(inv.Files) != 2000 {
		t.Fatalf("expected exactly 2000 files, got %d", len(inv.Files))
	}
	if !inv.Truncated {
		t.Errorf("expected inventory Truncated=true on 2500 file input")
	}

	foundFileBound := false
	for _, om := range inv.Omissions {
		if strings.Contains(om.Reason, "DIFF_MAX_FILES") {
			foundFileBound = true
			break
		}
	}
	if !foundFileBound {
		t.Errorf("expected omission naming DIFF_MAX_FILES, got omissions: %+v", inv.Omissions)
	}
}
