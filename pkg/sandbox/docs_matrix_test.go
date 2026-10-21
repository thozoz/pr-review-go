package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docsMatrixPath is the published verification matrix, relative to the package
// directory (package tests run with pkg/sandbox as the working directory).
const docsMatrixPath = "../../docs/SANDBOX.md"

const (
	docsMatrixStartMarker = "<!-- sandbox-verification-matrix:start -->"
	docsMatrixEndMarker   = "<!-- sandbox-verification-matrix:end -->"
)

// docsMatrixTestName matches a Go test identifier inside the matrix block.
var docsMatrixTestName = regexp.MustCompile(`\bTest[A-Za-z0-9_]+\b`)

// docsTestDeclaration matches a top-level test function declaration in a
// package _test.go file.
var docsTestDeclaration = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)

// TestDocsVerificationMatrixNamesRealTests is the doc-drift guard (T-12-04):
// every Go test identifier published in the marker-delimited verification
// matrix in docs/SANDBOX.md must really be declared as a `func Test…(` in this
// package's _test.go files. It extracts names ONLY from between the markers, so
// prose elsewhere in the document cannot satisfy or break it, and it refuses a
// gutted block by requiring a floor of distinct proofs. It executes no other
// test and makes no Landlock syscall.
func TestDocsVerificationMatrixNamesRealTests(t *testing.T) {
	raw, err := os.ReadFile(docsMatrixPath)
	if err != nil {
		// /docs/ is a local-only, gitignored tree in this repository, so a
		// fresh clone (CI) has no docs/SANDBOX.md at all. That is an
		// environment fact, not doc drift: skip loudly rather than failing
		// every CI run. Every assertion below stays strict whenever the
		// document is present.
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("doc-drift guard inert: %s does not exist because /docs/ is a local-only, gitignored tree; run this in a working tree that carries the local docs to verify the matrix", docsMatrixPath)
		}
		t.Fatalf("read %s: %v", docsMatrixPath, err)
	}
	src := string(raw)

	if got := strings.Count(src, docsMatrixStartMarker); got != 1 {
		t.Fatalf("%s must contain exactly one %q marker, found %d", docsMatrixPath, docsMatrixStartMarker, got)
	}
	if got := strings.Count(src, docsMatrixEndMarker); got != 1 {
		t.Fatalf("%s must contain exactly one %q marker, found %d", docsMatrixPath, docsMatrixEndMarker, got)
	}
	start := strings.Index(src, docsMatrixStartMarker)
	end := strings.Index(src, docsMatrixEndMarker)
	if start < 0 || end < 0 || start >= end {
		t.Fatalf("%s matrix markers are missing or out of order (start=%d end=%d)", docsMatrixPath, start, end)
	}
	block := src[start+len(docsMatrixStartMarker) : end]

	// Deduplicate while preserving deterministic order.
	seen := make(map[string]struct{})
	var matrixNames []string
	for _, name := range docsMatrixTestName.FindAllString(block, -1) {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		matrixNames = append(matrixNames, name)
	}
	if len(matrixNames) < 10 {
		t.Fatalf("%s matrix block names only %d distinct proofs; it must name at least 10 (got: %v)",
			docsMatrixPath, len(matrixNames), matrixNames)
	}

	// Collect every test function declared in this package.
	declared := make(map[string]struct{})
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fileRaw, readErr := os.ReadFile(filepath.Join(".", entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		for _, match := range docsTestDeclaration.FindAllStringSubmatch(string(fileRaw), -1) {
			declared[match[1]] = struct{}{}
		}
	}

	var missing []string
	for _, name := range matrixNames {
		if _, ok := declared[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s matrix names %d test(s) that are not declared in pkg/sandbox/*_test.go: %v",
			docsMatrixPath, len(missing), missing)
	}

	// The matrix must keep naming the proofs that carry the milestone's
	// headline guarantees; a rename cannot silently drop them.
	for _, required := range []string{
		"TestHandledAccessFSThroughABI7",
		"TestHelperExitContractDisjoint",
		"TestDenialClassifierTruthTable",
		"TestNoServerCallsLandlockApplier",
		"TestBwrapROLiveProbe",
		"TestOnHostLandlockRankUnavailableReason",
	} {
		if _, ok := seen[required]; !ok {
			t.Errorf("%s matrix block must name %s", docsMatrixPath, required)
		}
	}
}
