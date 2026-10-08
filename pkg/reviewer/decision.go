package reviewer

import (
	"fmt"
	"strings"
	"sync"

	"github.com/thozoz/pr-review-go/pkg/dedup"
)

// PriorFinding represents a stored finding from a prior head review for reporting classification (D-11).
type PriorFinding struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Fingerprint string `json:"fingerprint"`
	HeadSHA     string `json:"head_sha"`
}

// PersistingFinding represents a finding from a prior head that persists on the current head,
// tolerating line shifts with old location attribution (D-11).
type PersistingFinding struct {
	Finding   Finding `json:"finding"`
	PriorFile string  `json:"prior_file"`
	PriorLine int     `json:"prior_line"`
	PriorSHA  string  `json:"prior_sha"`
}

// FixedFinding represents a finding from a prior head that is no longer detected (D-11).
type FixedFinding struct {
	Title    string `json:"title"`
	File     string `json:"file"`
	Severity string `json:"severity"`
	PriorSHA string `json:"prior_sha"`
}

// ClassifiedFindings partitions findings into persisting, fixed, and new classes (D-11).
type ClassifiedFindings struct {
	Persisting []PersistingFinding `json:"persisting"`
	Fixed      []FixedFinding      `json:"fixed"`
	New        []Finding           `json:"new"`
}

// NormalizeFindingTitle trims and lowercases title text for reporting identity matching.
func NormalizeFindingTitle(title string) string {
	return strings.ToLower(strings.TrimSpace(title))
}

func findingIdentity(file, title, severity string) string {
	return fmt.Sprintf("%s:%s:%s",
		strings.TrimSpace(file),
		NormalizeFindingTitle(title),
		strings.ToUpper(strings.TrimSpace(severity)),
	)
}

// ClassifyAgainstPrior classifies current findings against prior head findings.
// Identity for REPORTING is file plus normalized title plus severity with line-shift tolerance (D-11).
// Exact fingerprint suppression (D-10) is untouched and separate from this reporting classification.
func ClassifyAgainstPrior(current []Finding, priorHead string, priorRows []PriorFinding) ClassifiedFindings {
	var result ClassifiedFindings

	type priorEntry struct {
		row     PriorFinding
		matched bool
	}

	priorByIdentity := make(map[string][]*priorEntry)
	for i := range priorRows {
		r := priorRows[i]
		if r.HeadSHA == "" {
			r.HeadSHA = priorHead
		}
		id := findingIdentity(r.File, r.Title, r.Severity)
		priorByIdentity[id] = append(priorByIdentity[id], &priorEntry{row: r})
	}

	for _, cur := range current {
		id := findingIdentity(cur.File, cur.Title, cur.Severity)
		entries := priorByIdentity[id]
		var match *priorEntry
		for _, e := range entries {
			if !e.matched {
				match = e
				break
			}
		}

		if match != nil {
			match.matched = true
			result.Persisting = append(result.Persisting, PersistingFinding{
				Finding:   cur,
				PriorFile: match.row.File,
				PriorLine: match.row.Line,
				PriorSHA:  match.row.HeadSHA,
			})
		} else {
			result.New = append(result.New, cur)
		}
	}

	for _, entries := range priorByIdentity {
		for _, e := range entries {
			if !e.matched {
				result.Fixed = append(result.Fixed, FixedFinding{
					Title:    e.row.Title,
					File:     e.row.File,
					Severity: e.row.Severity,
					PriorSHA: e.row.HeadSHA,
				})
			}
		}
	}

	return result
}

// BuildPriorFindingRecords converts current findings into storage rows tagged with headSHA.
func BuildPriorFindingRecords(findings []Finding, headSHA string) []PriorFinding {
	out := make([]PriorFinding, 0, len(findings))
	for _, f := range findings {
		fp := dedup.ComputeFingerprint(f.File, f.Line, f.Title, f.Severity)
		out = append(out, PriorFinding{
			File:        f.File,
			Line:        f.Line,
			Severity:    f.Severity,
			Title:       f.Title,
			Fingerprint: fp,
			HeadSHA:     headSHA,
		})
	}
	return out
}

// FindingsLedger stores historical finding records per PR and head commit.
type FindingsLedger struct {
	mu      sync.RWMutex
	records map[string]map[string][]PriorFinding // prKey -> headSHA -> findings
}

func NewFindingsLedger() *FindingsLedger {
	return &FindingsLedger{
		records: make(map[string]map[string][]PriorFinding),
	}
}

var defaultFindingsLedger = NewFindingsLedger()

// RecordFindings records findings for (prKey, headSHA) in the findings ledger.
func (l *FindingsLedger) RecordFindings(prKey string, headSHA string, findings []Finding) []PriorFinding {
	l.mu.Lock()
	defer l.mu.Unlock()

	rows := BuildPriorFindingRecords(findings, headSHA)
	if l.records[prKey] == nil {
		l.records[prKey] = make(map[string][]PriorFinding)
	}
	l.records[prKey][headSHA] = rows
	return rows
}

// GetFindings retrieves recorded prior findings for (prKey, headSHA).
func (l *FindingsLedger) GetFindings(prKey string, headSHA string) []PriorFinding {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if heads, ok := l.records[prKey]; ok {
		if rows, ok := heads[headSHA]; ok {
			cp := make([]PriorFinding, len(rows))
			copy(cp, rows)
			return cp
		}
	}
	return nil
}

// RecordFindings records findings in the default findings ledger.
func RecordFindings(prKey string, headSHA string, findings []Finding) []PriorFinding {
	return defaultFindingsLedger.RecordFindings(prKey, headSHA, findings)
}

// GetFindings retrieves findings from the default findings ledger.
func GetFindings(prKey string, headSHA string) []PriorFinding {
	return defaultFindingsLedger.GetFindings(prKey, headSHA)
}
