package docgen

import (
    "os"
    "path/filepath"
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
