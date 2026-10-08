package docgen

import (
    "errors"
    "os"
    "path/filepath"
    "runtime"
    "strings"
    "testing"
)

// helper source file for testing
const testSrc = `package testpkg

// DocumentedFunc has a doc comment.
func DocumentedFunc() {}

func UndocFunc() {}

// DocumentedStruct is a documented struct.
type DocumentedStruct struct {}

type UndocStruct struct {}
`

func TestFindUndocumentedGoItems(t *testing.T) {
    // create temporary dir
    dir, err := os.MkdirTemp("", "docgen_test")
    if err != nil {
        t.Fatalf("mktemp: %v", err)
    }
    defer os.RemoveAll(dir)

    // write test source file
    srcPath := filepath.Join(dir, "sample.go")
    if err := os.WriteFile(srcPath, []byte(testSrc), 0644); err != nil {
        t.Fatalf("write src: %v", err)
    }

    items, err := FindUndocumentedGoItems(dir)
    if err != nil {
        t.Fatalf("FindUndocumentedGoItems error: %v", err)
    }
    // Expect two undocumented items: UndocFunc and UndocStruct
    if len(items) != 2 {
        t.Fatalf("expected 2 undocumented items, got %d: %+v", len(items), items)
    }
    // check names
    names := map[string]bool{}
    for _, it := range items {
        names[it.Name] = true
    }
    if !names["UndocFunc"] || !names["UndocStruct"] {
        t.Fatalf("missing expected items in %v", names)
    }
}

// writeFixture creates dir/name with content, failing the test on error.
func writeFixture(t *testing.T, dir, name, content string) {
    t.Helper()
    if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
        t.Fatalf("write fixture %s: %v", name, err)
    }
}

// itemNames returns the set of "Kind/Name" keys for exact comparison.
func itemNames(items []UndocumentedItem) map[string]bool {
    names := map[string]bool{}
    for _, it := range items {
        names[it.Kind+"/"+it.Name] = true
    }
    return names
}

// TestFindUndocumentedGoItemsEdges pins D-14: each edge case asserts the exact
// item list. Scan scope stays top-level func plus type; no silent expansion.
func TestFindUndocumentedGoItemsEdges(t *testing.T) {
    tests := []struct {
        name     string
        filename string
        src      string
        want     map[string]bool
    }{
        {
            name:     "interface reported once methods not enumerated",
            filename: "iface.go",
            src: `package testpkg

type Doer interface {
    Do() error
    DoMore(x int) string
}
`,
            want: map[string]bool{"type/Doer": true},
        },
        {
            name:     "const and var blocks skipped",
            filename: "constvar.go",
            src: `package testpkg

const MaxRetries = 3

const (
    Alpha = 1
    Beta  = 2
)

var DefaultName = "x"

var (
    A = 1
    B = 2
)
`,
            want: map[string]bool{},
        },
        {
            name:     "generic func and type reported at declaration level",
            filename: "generics.go",
            src: `package testpkg

func MapSlice[T any, R any](in []T, fn func(T) R) []R { return nil }

type Pair[K comparable, V any] struct {
    Key K
    Val V
}
`,
            want: map[string]bool{"func/MapSlice": true, "type/Pair": true},
        },
        {
            name:     "already-documented excluded",
            filename: "documented.go",
            src: `package testpkg

// DocFunc has docs.
func DocFunc() {}

// DocType has docs.
type DocType struct{}
`,
            want: map[string]bool{},
        },
        {
            name:     "value and pointer receivers both covered",
            filename: "receivers.go",
            src: `package testpkg

// Widget has docs.
type Widget struct{}

func (w Widget) ValueMethod() {}

func (w *Widget) PointerMethod() {}
`,
            want: map[string]bool{"func/(Widget).ValueMethod": true, "func/(Widget).PointerMethod": true},
        },
        {
            name:     "test files scanned like other Go files",
            filename: "helper_test.go",
            src: `package testpkg

func TestHelperUndoc() {}
`,
            want: map[string]bool{"func/TestHelperUndoc": true},
        },
        {
            name:     "documented interface excluded",
            filename: "dociface.go",
            src: `package testpkg

// Doer has docs.
type Doer interface {
    Do() error
}
`,
            want: map[string]bool{},
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            dir, err := os.MkdirTemp("", "docgen_edge")
            if err != nil {
                t.Fatalf("mktemp: %v", err)
            }
            defer os.RemoveAll(dir)
            writeFixture(t, dir, tc.filename, tc.src)

            items, err := FindUndocumentedGoItems(dir)
            if err != nil {
                t.Fatalf("FindUndocumentedGoItems error: %v", err)
            }
            got := itemNames(items)
            if len(got) != len(tc.want) {
                t.Fatalf("expected exactly %v, got %+v", tc.want, items)
            }
            for k := range tc.want {
                if !got[k] {
                    t.Fatalf("missing expected item %q in %+v", k, items)
                }
            }
        })
    }
}

// TestNonGoFilesSkipped pins that non-Go files never contribute items.
func TestNonGoFilesSkipped(t *testing.T) {
    dir, err := os.MkdirTemp("", "docgen_nongo")
    if err != nil {
        t.Fatalf("mktemp: %v", err)
    }
    defer os.RemoveAll(dir)
    writeFixture(t, dir, "notes.txt", "func FakeUndoc() {}\ntype FakeType struct{}\n")
    writeFixture(t, dir, "script.md", "# func FakeUndoc\n")
    writeFixture(t, dir, "real.go", "package testpkg\n\nfunc RealUndoc() {}\n")

    items, err := FindUndocumentedGoItems(dir)
    if err != nil {
        t.Fatalf("FindUndocumentedGoItems error: %v", err)
    }
    got := itemNames(items)
    if len(got) != 1 || !got["func/RealUndoc"] {
        t.Fatalf("expected only func/RealUndoc, got %+v", items)
    }
}

// TestUnparseableFileErrorPropagation pins that a broken Go file surfaces an
// error instead of silently returning partial results.
func TestUnparseableFileErrorPropagation(t *testing.T) {
    dir, err := os.MkdirTemp("", "docgen_broken")
    if err != nil {
        t.Fatalf("mktemp: %v", err)
    }
    defer os.RemoveAll(dir)
    writeFixture(t, dir, "broken.go", "package testpkg\n\nfunc Broken( {\n")

    items, err := FindUndocumentedGoItems(dir)
    if err == nil {
        t.Fatalf("expected parse error for unparseable file, got items %+v", items)
    }
    if items != nil {
        t.Fatalf("expected nil items on parse error, got %+v", items)
    }
}

// TestApplyDocsHardGate pins D-13: ApplyDocs is permanently disabled and
// always returns ErrApplyDocsDisabled.
func TestApplyDocsHardGate(t *testing.T) {
    if err := ApplyDocs(nil); !errors.Is(err, ErrApplyDocsDisabled) {
        t.Fatalf("expected ErrApplyDocsDisabled for nil items, got %v", err)
    }
    items := []UndocumentedItem{{Name: "Foo", Kind: "func", File: "foo.go"}}
    if err := ApplyDocs(items); !errors.Is(err, ErrApplyDocsDisabled) {
        t.Fatalf("expected ErrApplyDocsDisabled for items, got %v", err)
    }
}

// TestServerDocsPathNeverMutates pins D-13/D-18: the server docs path
// (dispatchAddDocs in pkg/server/server.go) must never call ApplyDocs,
// UpdateFile, or any commit/push helper. Verified by grepping the live
// server source from within the test; the report-only PostComment call
// must remain the only write on that path.
func TestServerDocsPathNeverMutates(t *testing.T) {
    _, thisFile, _, ok := runtime.Caller(0)
    if !ok {
        t.Fatal("runtime.Caller failed")
    }
    serverPath := filepath.Join(filepath.Dir(thisFile), "..", "server", "server.go")
    src, err := os.ReadFile(serverPath)
    if err != nil {
        t.Fatalf("read server.go: %v", err)
    }
    const fnMarker = "func (s *Server) dispatchAddDocs"
    start := strings.Index(string(src), fnMarker)
    if start < 0 {
        t.Fatalf("dispatchAddDocs not found in %s", serverPath)
    }
    rest := string(src)[start:]
    // Slice the function body: up to the next top-level func.
    end := strings.Index(rest[len(fnMarker):], "\nfunc ")
    var body string
    if end < 0 {
        body = rest
    } else {
        body = rest[:len(fnMarker)+end]
    }

    for _, forbidden := range []string{
        "ApplyDocs",
        "UpdateFile",
        "CommitFileAtExpectedHead",
        "createCommitOnBranch",
        "commit_and_push",
        ".Push(",
    } {
        if strings.Contains(body, forbidden) {
            t.Errorf("dispatchAddDocs must never reference %q (D-13/D-18)", forbidden)
        }
    }
    if !strings.Contains(body, "PostComment") {
        t.Errorf("dispatchAddDocs must post its report via PostComment (report-only path)")
    }
}
