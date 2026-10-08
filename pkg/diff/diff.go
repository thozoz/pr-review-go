package diff

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/thozoz/pr-review-go/pkg/config"
)

// Hunk represents a single unified diff hunk (@@ ... @@).
type Hunk struct {
	ID         string       `json:"id"`
	File       string       `json:"file"`
	Header     string       `json:"header"`
	OldStart   int          `json:"old_start"`
	OldCount   int          `json:"old_count"`
	NewStart   int          `json:"new_start"`
	NewCount   int          `json:"new_count"`
	Body       string       `json:"body"`
	Lines      []string     `json:"lines"`
	AddedLines map[int]bool `json:"added_lines"` // line numbers on RIGHT side that were added (+)
}

// FileDiff represents changes to a single file.
type FileDiff struct {
	Path        string   `json:"path"`
	OldPath     string   `json:"old_path,omitempty"`
	IsNew       bool     `json:"is_new,omitempty"`
	IsDeleted   bool     `json:"is_deleted,omitempty"`
	IsRename    bool     `json:"is_rename,omitempty"`
	Similarity  int      `json:"similarity,omitempty"`
	IsBinary    bool     `json:"is_binary,omitempty"`
	IsModeOnly  bool     `json:"is_mode_only,omitempty"`
	OldMode     string   `json:"old_mode,omitempty"`
	NewMode     string   `json:"new_mode,omitempty"`
	HeaderLines []string `json:"header_lines,omitempty"`
	Hunks       []*Hunk  `json:"hunks"`
}

// Omission records changes or hunks that could not be parsed, exceeded caps, or were omitted from windows.
type Omission struct {
	File   string `json:"file,omitempty"`
	HunkID string `json:"hunk_id,omitempty"`
	Reason string `json:"reason"`
}

// ChangeInventory holds the parsed diff structure and omission records.
type ChangeInventory struct {
	Files      []*FileDiff  `json:"files"`
	Omissions  []*Omission  `json:"omissions"`
	TotalHunks int          `json:"total_hunks"`
	TotalFiles int          `json:"total_files"`
	Truncated  bool         `json:"truncated"`
}

// ParseOptions configures limits for diff parsing.
type ParseOptions struct {
	MaxBytes int64
	MaxFiles int
	MaxHunks int
}

// DefaultParseOptions returns the standard defaults.
func DefaultParseOptions() ParseOptions {
	return ParseOptions{
		MaxBytes: config.DefaultDiffMaxBytes,
		MaxFiles: config.DefaultDiffMaxFiles,
		MaxHunks: config.DefaultDiffMaxHunks,
	}
}

// ParseOptionsFromConfig builds ParseOptions from a loaded config.
func ParseOptionsFromConfig(cfg *config.Config) ParseOptions {
	if cfg == nil {
		return DefaultParseOptions()
	}
	opts := DefaultParseOptions()
	if cfg.DiffMaxBytes > 0 {
		opts.MaxBytes = cfg.DiffMaxBytes
	}
	if cfg.DiffMaxFiles > 0 {
		opts.MaxFiles = cfg.DiffMaxFiles
	}
	if cfg.DiffMaxHunks > 0 {
		opts.MaxHunks = cfg.DiffMaxHunks
	}
	return opts
}

func resolveOptions(opts ...ParseOptions) ParseOptions {
	if len(opts) == 0 {
		return DefaultParseOptions()
	}
	o := opts[0]
	if o.MaxBytes <= 0 {
		o.MaxBytes = config.DefaultDiffMaxBytes
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = config.DefaultDiffMaxFiles
	}
	if o.MaxHunks <= 0 {
		o.MaxHunks = config.DefaultDiffMaxHunks
	}
	return o
}

// Parse parses a unified git diff into a ChangeInventory with hunk-atomic records and omission accounting.
func Parse(rawDiff string, options ...ParseOptions) *ChangeInventory {
	opts := resolveOptions(options...)
	inv := &ChangeInventory{
		Files:     []*FileDiff{},
		Omissions: []*Omission{},
	}

	trimmed := strings.TrimSpace(rawDiff)
	if trimmed == "" {
		return inv
	}

	// T-03-01-01: Bounded input parsing - enforce DIFF_MAX_BYTES before line splitting
	if opts.MaxBytes > 0 && int64(len(rawDiff)) > opts.MaxBytes {
		inv.Omissions = append(inv.Omissions, &Omission{
			Reason: fmt.Sprintf("exceeded diff size limit (DIFF_MAX_BYTES=%d)", opts.MaxBytes),
		})
		inv.Truncated = true
		cut := rawDiff[:opts.MaxBytes]
		// Snap to last newline to avoid partial line
		if lastNL := strings.LastIndexByte(cut, '\n'); lastNL > 0 {
			rawDiff = cut[:lastNL]
		} else {
			rawDiff = cut
		}
	}

	lines := strings.Split(rawDiff, "\n")
	var curFile *FileDiff
	var curHunk *Hunk
	var curHunkLines []string
	var curHunkAddedLines map[int]bool
	var curLine int
	fileCapHit := false
	hunkCapHit := false

	flushHunk := func() {
		if curHunk != nil && curFile != nil {
			curHunk.Lines = curHunkLines
			curHunk.AddedLines = curHunkAddedLines
			allLines := append([]string{curHunk.Header}, curHunkLines...)
			curHunk.Body = strings.Join(allLines, "\n")
			curFile.Hunks = append(curFile.Hunks, curHunk)
			inv.TotalHunks++
			curHunk = nil
			curHunkLines = nil
			curHunkAddedLines = nil
		}
	}

	flushFile := func() {
		flushHunk()
		if curFile != nil {
			if len(curFile.Hunks) == 0 && curFile.OldMode != "" && curFile.NewMode != "" && !curFile.IsBinary {
				curFile.IsModeOnly = true
			}
			inv.Files = append(inv.Files, curFile)
			inv.TotalFiles++
			curFile = nil
		}
	}

	for lineIdx := 0; lineIdx < len(lines); lineIdx++ {
		line := lines[lineIdx]

		// Check for new file diff header
		if strings.HasPrefix(line, "diff --git ") {
			if fileCapHit {
				continue
			}
			flushFile()
			if len(inv.Files) >= opts.MaxFiles {
				fileCapHit = true
				inv.Truncated = true
				inv.Omissions = append(inv.Omissions, &Omission{
					Reason: fmt.Sprintf("exceeded file limit (DIFF_MAX_FILES=%d)", opts.MaxFiles),
				})
				continue
			}

			curFile = &FileDiff{
				Hunks:       []*Hunk{},
				HeaderLines: []string{line},
			}

			fields := strings.Fields(line)
			if len(fields) >= 4 {
				oldPath := strings.TrimPrefix(fields[2], "a/")
				newPath := strings.TrimPrefix(fields[3], "b/")
				curFile.Path = newPath
				if oldPath != newPath {
					curFile.OldPath = oldPath
				}
			}
			continue
		}

		// Also handle standalone "--- a/" when not preceded by "diff --git"
		if strings.HasPrefix(line, "--- ") && curFile == nil {
			if fileCapHit {
				continue
			}
			if len(inv.Files) >= opts.MaxFiles {
				fileCapHit = true
				inv.Truncated = true
				inv.Omissions = append(inv.Omissions, &Omission{
					Reason: fmt.Sprintf("exceeded file limit (DIFF_MAX_FILES=%d)", opts.MaxFiles),
				})
				continue
			}
			curFile = &FileDiff{
				Hunks:       []*Hunk{},
				HeaderLines: []string{line},
			}
		}

		if fileCapHit {
			continue
		}

		// If we encounter headers inside a file diff
		if curFile != nil && curHunk == nil {
			if strings.HasPrefix(line, "old mode ") {
				curFile.OldMode = strings.TrimPrefix(line, "old mode ")
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "new mode ") {
				curFile.NewMode = strings.TrimPrefix(line, "new mode ")
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "new file mode ") {
				curFile.IsNew = true
				curFile.NewMode = strings.TrimPrefix(line, "new file mode ")
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "deleted file mode ") {
				curFile.IsDeleted = true
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "similarity index ") {
				simStr := strings.TrimSuffix(strings.TrimPrefix(line, "similarity index "), "%")
				if sim, err := strconv.Atoi(simStr); err == nil {
					curFile.Similarity = sim
				}
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "rename from ") {
				curFile.IsRename = true
				curFile.OldPath = strings.TrimPrefix(line, "rename from ")
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "rename to ") {
				curFile.IsRename = true
				curFile.Path = strings.TrimPrefix(line, "rename to ")
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch") {
				curFile.IsBinary = true
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				continue
			}
			if strings.HasPrefix(line, "--- ") {
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				path := strings.TrimPrefix(line, "--- ")
				if path != "/dev/null" {
					path = strings.TrimPrefix(path, "a/")
					if curFile.OldPath == "" && curFile.Path != "" && path != curFile.Path {
						curFile.OldPath = path
					}
				}
				continue
			}
			if strings.HasPrefix(line, "+++ ") {
				curFile.HeaderLines = append(curFile.HeaderLines, line)
				path := strings.TrimPrefix(line, "+++ ")
				if path != "/dev/null" {
					curFile.Path = strings.TrimPrefix(path, "b/")
				}
				continue
			}
		}

		// Check for hunk header @@ ... @@
		if strings.HasPrefix(line, "@@") {
			flushHunk()

			if curFile == nil {
				curFile = &FileDiff{
					Path:        "unknown",
					Hunks:       []*Hunk{},
					HeaderLines: []string{},
				}
			}

			if hunkCapHit {
				continue
			}
			if inv.TotalHunks >= opts.MaxHunks {
				hunkCapHit = true
				inv.Truncated = true
				inv.Omissions = append(inv.Omissions, &Omission{
					File:   curFile.Path,
					Reason: fmt.Sprintf("exceeded hunk limit (DIFF_MAX_HUNKS=%d)", opts.MaxHunks),
				})
				continue
			}

			oldStart, oldCount, newStart, newCount, ok := parseHunkHeader(line)
			if !ok {
				// Malformed hunk header is recorded as omission, never panics
				inv.Omissions = append(inv.Omissions, &Omission{
					File:   curFile.Path,
					Reason: fmt.Sprintf("malformed hunk header: %s", line),
				})
				continue
			}

			hunkID := fmt.Sprintf("%s#H%d", curFile.Path, len(curFile.Hunks)+1)
			curHunk = &Hunk{
				ID:       hunkID,
				File:     curFile.Path,
				Header:   line,
				OldStart: oldStart,
				OldCount: oldCount,
				NewStart: newStart,
				NewCount: newCount,
			}
			curHunkLines = []string{}
			curHunkAddedLines = make(map[int]bool)
			curLine = newStart
			continue
		}

		// Inside hunk lines
		if curHunk != nil {
			curHunkLines = append(curHunkLines, line)
			if len(line) == 0 {
				curLine++
				continue
			}
			switch line[0] {
			case '+':
				curHunkAddedLines[curLine] = true
				curLine++
			case '-':
				// Removed lines do not exist on RIGHT side of review
			case '\\':
				// \ No newline at end of file - does not advance line
			default:
				curLine++
			}
			continue
		}

		if curFile != nil {
			curFile.HeaderLines = append(curFile.HeaderLines, line)
		}
	}

	flushFile()
	return inv
}

func parseHunkHeader(line string) (oldStart, oldCount, newStart, newCount int, ok bool) {
	if !strings.HasPrefix(line, "@@") {
		return 0, 0, 0, 0, false
	}
	secondAt := strings.Index(line[2:], "@@")
	if secondAt < 0 {
		return 0, 0, 0, 0, false
	}
	ranges := strings.TrimSpace(line[2 : 2+secondAt])
	parts := strings.Fields(ranges)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "-") || !strings.HasPrefix(parts[1], "+") {
		return 0, 0, 0, 0, false
	}

	oldStr := strings.TrimPrefix(parts[0], "-")
	oldStartStr, oldCountStr, hasOldCount := strings.Cut(oldStr, ",")
	os, err := strconv.Atoi(oldStartStr)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	oldStart = os
	oldCount = 1
	if hasOldCount {
		oc, err := strconv.Atoi(oldCountStr)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		oldCount = oc
	}

	newStr := strings.TrimPrefix(parts[1], "+")
	newStartStr, newCountStr, hasNewCount := strings.Cut(newStr, ",")
	ns, err := strconv.Atoi(newStartStr)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	newStart = ns
	newCount = 1
	if hasNewCount {
		nc, err := strconv.Atoi(newCountStr)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		newCount = nc
	}

	return oldStart, oldCount, newStart, newCount, true
}

// ChangedPRLines returns a map of file -> line -> true for all added lines on the new (RIGHT) side.
// Matches changedPRLines semantics from reviewer engine.
func (inv *ChangeInventory) ChangedPRLines() map[string]map[int]bool {
	lines := make(map[string]map[int]bool)
	for _, f := range inv.Files {
		for _, h := range f.Hunks {
			if len(h.AddedLines) > 0 {
				if lines[f.Path] == nil {
					lines[f.Path] = make(map[int]bool)
				}
				for l := range h.AddedLines {
					lines[f.Path][l] = true
				}
			}
		}
	}
	return lines
}

// FindFile returns the FileDiff for the given path, or nil if not found.
func (inv *ChangeInventory) FindFile(path string) *FileDiff {
	for _, f := range inv.Files {
		if f.Path == path || f.OldPath == path {
			return f
		}
	}
	return nil
}

// HunksForFile returns all hunks for the given file path.
func (inv *ChangeInventory) HunksForFile(path string) []*Hunk {
	if f := inv.FindFile(path); f != nil {
		return f.Hunks
	}
	return nil
}

// DiffWindow represents a hunk-atomic window of changes within a byte budget.
// A window NEVER slices across or inside a hunk.
type DiffWindow struct {
	Index     int          `json:"index"`
	Hunks     []*Hunk      `json:"hunks"`
	Files     []string     `json:"files"`
	Content   string       `json:"content"`
	ByteCount int          `json:"byte_count"`
	Truncated bool         `json:"truncated"`
	Omissions []*Omission  `json:"omissions,omitempty"`
}

// Windows creates one or more hunk-atomic windows under the given byte budget.
// D-05: diff windowing never cuts inside a hunk. Oversized hunks become omission records.
func (inv *ChangeInventory) Windows(maxBytes int) []*DiffWindow {
	if maxBytes <= 0 {
		maxBytes = int(config.DefaultDiffMaxBytes)
	}

	var windows []*DiffWindow
	var curHunks []*Hunk
	var curFiles []string
	seenFiles := make(map[string]bool)
	var curContent strings.Builder
	var curOmissions []*Omission

	finishWindow := func() {
		if len(curHunks) > 0 || curContent.Len() > 0 {
			w := &DiffWindow{
				Index:     len(windows),
				Hunks:     curHunks,
				Files:     curFiles,
				Content:   curContent.String(),
				ByteCount: curContent.Len(),
				Omissions: curOmissions,
			}
			windows = append(windows, w)
			curHunks = nil
			curFiles = nil
			seenFiles = make(map[string]bool)
			curContent.Reset()
			curOmissions = nil
		}
	}

	// Include pre-existing inventory omissions in the window tracking
	for _, om := range inv.Omissions {
		curOmissions = append(curOmissions, om)
	}

	for _, f := range inv.Files {
		if f.IsBinary {
			curOmissions = append(curOmissions, &Omission{
				File:   f.Path,
				Reason: "binary file",
			})
			continue
		}
		if f.IsModeOnly {
			curOmissions = append(curOmissions, &Omission{
				File:   f.Path,
				Reason: "mode change only",
			})
			continue
		}
		if len(f.Hunks) == 0 {
			continue
		}

		fileHeader := fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", f.Path, f.Path, f.Path, f.Path)
		if f.OldPath != "" {
			fileHeader = fmt.Sprintf("diff --git a/%s b/%s\nrename from %s\nrename to %s\n--- a/%s\n+++ b/%s\n",
				f.OldPath, f.Path, f.OldPath, f.Path, f.OldPath, f.Path)
		}

		fileHeaderIncluded := false

		for _, h := range f.Hunks {
			hunkText := h.Header + "\n" + strings.Join(h.Lines, "\n") + "\n"
			headerCost := 0
			if !fileHeaderIncluded && !seenFiles[f.Path] {
				headerCost = len(fileHeader)
			}
			cost := headerCost + len(hunkText)

			// If a single hunk plus header exceeds the entire window budget by itself,
			// D-05: never split inside the hunk. It becomes an omission.
			if cost > maxBytes && curContent.Len() == 0 {
				curOmissions = append(curOmissions, &Omission{
					File:   f.Path,
					HunkID: h.ID,
					Reason: fmt.Sprintf("hunk %s exceeds window byte budget (%d > %d)", h.ID, cost, maxBytes),
				})
				continue
			}

			// If it exceeds current window capacity, finish current window and start a new one
			if curContent.Len()+cost > maxBytes && curContent.Len() > 0 {
				finishWindow()
				headerCost = 0
				if !seenFiles[f.Path] {
					headerCost = len(fileHeader)
				}
				cost = headerCost + len(hunkText)
				// Re-check single hunk oversized condition for fresh window
				if cost > maxBytes {
					curOmissions = append(curOmissions, &Omission{
						File:   f.Path,
						HunkID: h.ID,
						Reason: fmt.Sprintf("hunk %s exceeds window byte budget (%d > %d)", h.ID, cost, maxBytes),
					})
					continue
				}
			}

			if !fileHeaderIncluded && !seenFiles[f.Path] {
				curContent.WriteString(fileHeader)
				fileHeaderIncluded = true
				seenFiles[f.Path] = true
				curFiles = append(curFiles, f.Path)
			}
			curContent.WriteString(hunkText)
			curHunks = append(curHunks, h)
		}
	}

	finishWindow()

	if len(windows) == 0 {
		windows = append(windows, &DiffWindow{
			Index:     0,
			Hunks:     []*Hunk{},
			Files:     []string{},
			Content:   "",
			ByteCount: 0,
			Truncated: inv.Truncated || len(curOmissions) > 0,
			Omissions: curOmissions,
		})
	}

	// Mark truncation flag across windows if inventory was truncated or multiple windows exist
	totalWindows := len(windows)
	for i, w := range windows {
		if inv.Truncated || totalWindows > 1 || len(w.Omissions) > 0 || i < totalWindows-1 {
			w.Truncated = true
		}
	}

	return windows
}

// Window returns the first hunk-atomic window under maxBytes.
// If any hunks or files could not fit or were omitted, Truncated is true.
func (inv *ChangeInventory) Window(maxBytes int) *DiffWindow {
	wins := inv.Windows(maxBytes)
	if len(wins) == 0 {
		return &DiffWindow{
			Truncated: inv.Truncated || len(inv.Omissions) > 0,
			Omissions: inv.Omissions,
		}
	}
	w := wins[0]
	if len(wins) > 1 || inv.Truncated {
		w.Truncated = true
	}
	return w
}

// Windows is a package-level helper to parse and window a raw diff.
func Windows(rawDiff string, maxBytes int, opts ...ParseOptions) ([]*DiffWindow, *ChangeInventory) {
	inv := Parse(rawDiff, opts...)
	return inv.Windows(maxBytes), inv
}

// LedgerEntry represents examination status of a single hunk or omission.
type LedgerEntry struct {
	HunkID string `json:"hunk_id"`
	File   string `json:"file"`
	Status string `json:"status"` // "examined" or "skipped"
	Reason string `json:"reason,omitempty"`
}

// CoverageLedger tracks hunk-level examination and omission accounting across review plans.
// D-05: CoverageLedger starts every hunk as skipped with reason "not examined".
type CoverageLedger struct {
	mu      sync.RWMutex
	entries []*LedgerEntry
	byID    map[string]*LedgerEntry
	byFile  map[string][]*LedgerEntry
}

// NewCoverageLedger initializes a ledger where every hunk and omission starts as skipped.
func NewCoverageLedger(inv *ChangeInventory) *CoverageLedger {
	ledger := &CoverageLedger{
		entries: []*LedgerEntry{},
		byID:    make(map[string]*LedgerEntry),
		byFile:  make(map[string][]*LedgerEntry),
	}
	if inv == nil {
		return ledger
	}

	for _, f := range inv.Files {
		if f.IsBinary {
			ledger.addEntry(&LedgerEntry{
				HunkID: f.Path + ":binary",
				File:   f.Path,
				Status: "skipped",
				Reason: "binary file",
			})
			continue
		}
		if f.IsModeOnly {
			ledger.addEntry(&LedgerEntry{
				HunkID: f.Path + ":mode-only",
				File:   f.Path,
				Status: "skipped",
				Reason: "mode change only",
			})
			continue
		}
		if len(f.Hunks) == 0 {
			reason := "no content changes"
			if f.IsRename {
				reason = fmt.Sprintf("renamed from %s (no content changes)", f.OldPath)
			}
			ledger.addEntry(&LedgerEntry{
				HunkID: f.Path + ":empty",
				File:   f.Path,
				Status: "skipped",
				Reason: reason,
			})
			continue
		}
		for _, h := range f.Hunks {
			ledger.addEntry(&LedgerEntry{
				HunkID: h.ID,
				File:   f.Path,
				Status: "skipped",
				Reason: "not examined",
			})
		}
	}

	for idx, om := range inv.Omissions {
		hunkID := om.HunkID
		if hunkID == "" {
			if om.File != "" {
				hunkID = fmt.Sprintf("%s:omission-%d", om.File, idx+1)
			} else {
				hunkID = fmt.Sprintf("omission-%d", idx+1)
			}
		}
		ledger.addEntry(&LedgerEntry{
			HunkID: hunkID,
			File:   om.File,
			Status: "skipped",
			Reason: om.Reason,
		})
	}

	return ledger
}

func (l *CoverageLedger) addEntry(e *LedgerEntry) {
	l.entries = append(l.entries, e)
	l.byID[e.HunkID] = e
	if e.File != "" {
		l.byFile[e.File] = append(l.byFile[e.File], e)
	}
}

// MarkExamined flips the given hunk ID to examined status.
func (l *CoverageLedger) MarkExamined(hunkID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byID[hunkID]; ok {
		e.Status = "examined"
		e.Reason = ""
	}
}

// MarkFileExamined flips all hunks of the given file to examined status (omissions remain skipped).
func (l *CoverageLedger) MarkFileExamined(file string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.byFile[file] {
		// Only examine standard hunks, not omission records
		if !strings.Contains(e.HunkID, "omission") && e.Reason == "not examined" {
			e.Status = "examined"
			e.Reason = ""
		}
	}
}

// MarkSkipped sets status to skipped with the provided reason.
func (l *CoverageLedger) MarkSkipped(hunkID, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byID[hunkID]; ok {
		e.Status = "skipped"
		e.Reason = reason
	}
}

// MarkFileSkipped sets all hunks of the file to skipped with reason.
func (l *CoverageLedger) MarkFileSkipped(file, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.byFile[file] {
		e.Status = "skipped"
		e.Reason = reason
	}
}

// IsExamined returns true if the hunk was examined.
func (l *CoverageLedger) IsExamined(hunkID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if e, ok := l.byID[hunkID]; ok {
		return e.Status == "examined"
	}
	return false
}

// Examined returns all examined ledger entries.
func (l *CoverageLedger) Examined() []*LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var res []*LedgerEntry
	for _, e := range l.entries {
		if e.Status == "examined" {
			res = append(res, e)
		}
	}
	return res
}

// Skipped returns all skipped ledger entries.
func (l *CoverageLedger) Skipped() []*LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var res []*LedgerEntry
	for _, e := range l.entries {
		if e.Status == "skipped" {
			res = append(res, e)
		}
	}
	return res
}

// Entries returns a copy of all ledger entries.
func (l *CoverageLedger) Entries() []*LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	res := make([]*LedgerEntry, len(l.entries))
	copy(res, l.entries)
	return res
}

// TotalHunks returns total entries tracked.
func (l *CoverageLedger) TotalHunks() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// ExaminedCount returns the number of examined entries.
func (l *CoverageLedger) ExaminedCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := 0
	for _, e := range l.entries {
		if e.Status == "examined" {
			count++
		}
	}
	return count
}

// SkippedCount returns the number of skipped entries.
func (l *CoverageLedger) SkippedCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	count := 0
	for _, e := range l.entries {
		if e.Status == "skipped" {
			count++
		}
	}
	return count
}

// IsComplete returns true if every tracked entry was examined with zero skipped.
func (l *CoverageLedger) IsComplete() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.entries) == 0 {
		return true
	}
	for _, e := range l.entries {
		if e.Status != "examined" {
			return false
		}
	}
	return true
}

// Summary returns examined and skipped counts.
func (l *CoverageLedger) Summary() (examined, skipped int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.Status == "examined" {
			examined++
		} else {
			skipped++
		}
	}
	return examined, skipped
}
