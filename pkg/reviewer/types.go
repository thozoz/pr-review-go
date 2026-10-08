package reviewer

import (
	"github.com/thozoz/pr-review-go/pkg/diff"
	"github.com/thozoz/pr-review-go/pkg/github"
	"github.com/thozoz/pr-review-go/pkg/sandbox"
)

type Finding struct {
	File          string `json:"file"`
	Line          int    `json:"line"`
	Severity      string `json:"severity"` // "CRITICAL", "WARNING", "NOTE"
	Title         string `json:"title"`
	Description   string `json:"description"`
	Suggestion    string `json:"suggestion,omitempty"`
	SuggestedCode string `json:"suggested_code,omitempty"`
}

type CommentTracking struct {
	ThreadKey string `json:"thread_key"`
	Status    string `json:"status"` // "ADDRESSED", "STILL_OPEN", "DISMISSED"
	Note      string `json:"note"`
}

type LLMReviewOutput struct {
	Score            int               `json:"score"` // 0 - 100
	Summary          string            `json:"summary"`
	CommentFollowups []CommentTracking `json:"comment_followups,omitempty"`
	Findings         []Finding         `json:"findings"`
}

type CoverageState string

const (
	CoverageStateFull    CoverageState = "full"
	CoverageStatePartial CoverageState = "partial"
)

type CoverageItem struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type CoverageReport struct {
	State         CoverageState  `json:"state"` // "full" or "partial"
	TotalHunks    int            `json:"total_hunks"`
	ExaminedCount int            `json:"examined_count"`
	SkippedCount  int            `json:"skipped_count"`
	ExaminedHunks []string       `json:"examined_hunks,omitempty"`
	SkippedHunks  []CoverageItem `json:"skipped_hunks,omitempty"`
	Omissions     []CoverageItem `json:"omissions,omitempty"`
}

type ReviewReport struct {
	PRNumber            int
	PRTitle             string
	HeadSHA             string
	Score               int
	Summary             string
	VerificationSummary string
	VerificationStatus  sandbox.VerificationStatus
	VerificationReason  string
	RulesSource         string
	DeduplicatedCount   int
	CommentFollowups    []CommentTracking
	Findings            []Finding
	Suggestions         []github.InlineSuggestion
	RawMarkdown         string
	Ledger              *diff.CoverageLedger
	Coverage            *CoverageReport
}

// GetCoverage returns report.Coverage if set, or derives it from report.Ledger.
func (r *ReviewReport) GetCoverage() *CoverageReport {
	if r == nil {
		return nil
	}
	if r.Coverage != nil {
		return r.Coverage
	}
	if r.Ledger == nil {
		return nil
	}
	return CoverageFromLedger(r.Ledger)
}

// CoverageFromLedger derives a CoverageReport from a diff.CoverageLedger.
func CoverageFromLedger(l *diff.CoverageLedger) *CoverageReport {
	if l == nil {
		return nil
	}
	total := l.TotalHunks()
	examinedCount := l.ExaminedCount()
	skippedCount := l.SkippedCount()

	state := CoverageStateFull
	if skippedCount > 0 || !l.IsComplete() {
		state = CoverageStatePartial
	}

	var examined []string
	var skipped []CoverageItem
	var omissions []CoverageItem

	for _, e := range l.Entries() {
		if e.Status == "examined" {
			examined = append(examined, e.HunkID)
		} else {
			item := CoverageItem{
				ID:     e.HunkID,
				File:   e.File,
				Reason: e.Reason,
			}
			if isOmissionEntry(e.HunkID, e.Reason) {
				omissions = append(omissions, item)
			} else {
				skipped = append(skipped, item)
			}
		}
	}

	return &CoverageReport{
		State:         state,
		TotalHunks:    total,
		ExaminedCount: examinedCount,
		SkippedCount:  skippedCount,
		ExaminedHunks: examined,
		SkippedHunks:  skipped,
		Omissions:     omissions,
	}
}

func isOmissionEntry(hunkID, reason string) bool {
	if hunkID == "" {
		return true
	}
	// Omissions from plan-01 diff parser include binary, mode-only, empty, and explicit omission IDs
	if len(hunkID) > 8 && hunkID[:8] == "omission" {
		return true
	}
	for i := 0; i+9 <= len(hunkID); i++ {
		if hunkID[i:i+9] == ":omission" {
			return true
		}
	}
	for i := 0; i+7 <= len(hunkID); i++ {
		if hunkID[i:i+7] == ":binary" {
			return true
		}
	}
	for i := 0; i+10 <= len(hunkID); i++ {
		if hunkID[i:i+10] == ":mode-only" {
			return true
		}
	}
	for i := 0; i+6 <= len(hunkID); i++ {
		if hunkID[i:i+6] == ":empty" {
			return true
		}
	}
	return false
}
