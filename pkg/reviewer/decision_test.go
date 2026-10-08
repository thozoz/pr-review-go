package reviewer

import (
	"testing"

	"github.com/thozoz/pr-review-go/pkg/dedup"
)

func TestClassify_ThreeHeadFixture(t *testing.T) {
	const (
		shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		shaC = "cccccccccccccccccccccccccccccccccccccccc"
	)

	// Head A introduces two findings:
	// Finding 1: Auth check missing (file: auth.go, line: 10, severity: CRITICAL)
	// Finding 2: Unchecked error (file: db.go, line: 50, severity: WARNING)
	findingsA := []Finding{
		{
			File:        "auth.go",
			Line:        10,
			Severity:    "CRITICAL",
			Title:       "Auth check missing",
			Description: "Nil token check missing in auth handler",
		},
		{
			File:        "db.go",
			Line:        50,
			Severity:    "WARNING",
			Title:       "Unchecked error",
			Description: "db.Close error ignored",
		},
	}

	recordsA := BuildPriorFindingRecords(findingsA, shaA)

	// Head B:
	// - Finding 1 shifted by lines (line 10 -> line 28)
	// - Finding 2 is resolved/fixed (absent)
	// - Finding 3 is brand new
	findingsB := []Finding{
		{
			File:        "auth.go",
			Line:        28, // line shifted!
			Severity:    "CRITICAL",
			Title:       "Auth check missing",
			Description: "Nil token check still missing in auth handler",
		},
		{
			File:        "api.go",
			Line:        99,
			Severity:    "NOTE",
			Title:       "Missing header doc",
			Description: "Header not documented in OpenAPI",
		},
	}

	classified := ClassifyAgainstPrior(findingsB, shaA, recordsA)

	// Assertions for Head B classification (D-11)
	if len(classified.Persisting) != 1 {
		t.Fatalf("expected 1 persisting finding, got %d", len(classified.Persisting))
	}
	p := classified.Persisting[0]
	if p.Finding.File != "auth.go" || p.Finding.Line != 28 {
		t.Errorf("expected current finding on auth.go:28, got %s:%d", p.Finding.File, p.Finding.Line)
	}
	if p.PriorFile != "auth.go" || p.PriorLine != 10 || p.PriorSHA != shaA {
		t.Errorf("expected prior attribution auth.go:10 in %s, got %s:%d in %s", shaA, p.PriorFile, p.PriorLine, p.PriorSHA)
	}

	if len(classified.Fixed) != 1 {
		t.Fatalf("expected 1 fixed finding, got %d", len(classified.Fixed))
	}
	fix := classified.Fixed[0]
	if fix.Title != "Unchecked error" || fix.File != "db.go" || fix.PriorSHA != shaA {
		t.Errorf("unexpected fixed finding details: %+v", fix)
	}

	if len(classified.New) != 1 {
		t.Fatalf("expected 1 new finding, got %d", len(classified.New))
	}
	newF := classified.New[0]
	if newF.Title != "Missing header doc" || newF.File != "api.go" {
		t.Errorf("unexpected new finding details: %+v", newF)
	}

	// Now Head C fixes the remaining persisting finding
	recordsB := BuildPriorFindingRecords(findingsB, shaB)
	findingsC := []Finding{
		{
			File:        "api.go",
			Line:        105, // Finding 3 persisted and shifted
			Severity:    "NOTE",
			Title:       "Missing header doc",
			Description: "Header still not documented",
		},
	}

	classifiedC := ClassifyAgainstPrior(findingsC, shaB, recordsB)
	if len(classifiedC.Persisting) != 1 {
		t.Fatalf("expected 1 persisting in C, got %d", len(classifiedC.Persisting))
	}
	if len(classifiedC.Fixed) != 1 {
		t.Fatalf("expected 1 fixed in C (auth check fixed), got %d", len(classifiedC.Fixed))
	}
	if classifiedC.Fixed[0].Title != "Auth check missing" {
		t.Errorf("expected Auth check missing to be fixed in C, got: %s", classifiedC.Fixed[0].Title)
	}
}

func TestClassify_ExactSuppressionPreserved(t *testing.T) {
	// D-10: exact fingerprint suppression sha256(file:line:title:severity)
	// A line shift or retitled finding must NOT be suppressed by fingerprint equality!
	fpA := dedup.ComputeFingerprint("auth.go", 10, "Auth check missing", "CRITICAL")
	fpShifted := dedup.ComputeFingerprint("auth.go", 28, "Auth check missing", "CRITICAL")
	fpRetitled := dedup.ComputeFingerprint("auth.go", 10, "Auth check missing completely", "CRITICAL")

	if fpA == fpShifted {
		t.Fatalf("D-10 violation: line shift produced identical fingerprint")
	}
	if fpA == fpRetitled {
		t.Fatalf("D-10 violation: retitled finding produced identical fingerprint")
	}
}

func TestRecordFindings_CrossPRIsolation(t *testing.T) {
	ledger := NewFindingsLedger()

	findingsPR1 := []Finding{
		{File: "pr1.go", Line: 10, Title: "Issue 1", Severity: "WARNING"},
	}
	findingsPR2 := []Finding{
		{File: "pr2.go", Line: 20, Title: "Issue 2", Severity: "CRITICAL"},
	}

	ledger.RecordFindings("github.com/1/1", "sha-1", findingsPR1)
	ledger.RecordFindings("github.com/1/2", "sha-1", findingsPR2)

	gotPR1 := ledger.GetFindings("github.com/1/1", "sha-1")
	if len(gotPR1) != 1 || gotPR1[0].File != "pr1.go" {
		t.Fatalf("expected PR1 findings, got: %+v", gotPR1)
	}

	gotPR2 := ledger.GetFindings("github.com/1/2", "sha-1")
	if len(gotPR2) != 1 || gotPR2[0].File != "pr2.go" {
		t.Fatalf("expected PR2 findings, got: %+v", gotPR2)
	}

	// PR 3 has none
	gotPR3 := ledger.GetFindings("github.com/1/3", "sha-1")
	if len(gotPR3) != 0 {
		t.Fatalf("expected empty findings for PR 3, got: %+v", gotPR3)
	}
}
