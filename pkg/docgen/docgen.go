package docgen

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrApplyDocsDisabled marks ApplyDocs as permanently disabled (D-13).
	// Documentation generation in pr-review-go is strictly report-only via PR comments.
	ErrApplyDocsDisabled = errors.New("ApplyDocs is permanently disabled: docgen is report-only (D-13)")
)

// UndocumentedItem represents a top-level Go declaration lacking a doc comment.
type UndocumentedItem struct {
    Name string // function, method, or type name
    Kind string // "func" or "type"
    File string // file where it appears
}

// FindUndocumentedGoItems scans the given directory recursively for .go files and returns items lacking preceding doc comments.
func FindUndocumentedGoItems(root string) ([]UndocumentedItem, error) {
    var items []UndocumentedItem
    fset := token.NewFileSet()
    err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        if info.IsDir() || !strings.HasSuffix(path, ".go") {
            return nil
        }
        src, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
        if err != nil {
            return err
        }
        for _, decl := range src.Decls {
            switch d := decl.(type) {
            case *ast.FuncDecl:
                if d.Doc == nil {
                    name := d.Name.Name
                    // for methods include receiver type
                    if d.Recv != nil && len(d.Recv.List) > 0 {
                        if star, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
                            if ident, ok := star.X.(*ast.Ident); ok {
                                name = "(" + ident.Name + ")" + "." + name
                            }
                        } else if ident, ok := d.Recv.List[0].Type.(*ast.Ident); ok {
                            name = "(" + ident.Name + ")" + "." + name
                        }
                    }
                    items = append(items, UndocumentedItem{Name: name, Kind: "func", File: path})
                }
            case *ast.GenDecl:
                if d.Tok == token.TYPE {
                    for _, spec := range d.Specs {
                        ts := spec.(*ast.TypeSpec)
                        if d.Doc == nil && ts.Doc == nil {
                            items = append(items, UndocumentedItem{Name: ts.Name.Name, Kind: "type", File: path})
                        }
                    }
                }
            }
        }
        return nil
    })
    if err != nil {
        return nil, err
    }
    return items, nil
}

// GenerateGoDoc returns a Go comment string for the given item.
func GenerateGoDoc(item UndocumentedItem) string {
    // simple placeholder doc
    if item.Kind == "func" {
        return "// " + item.Name + " ..."
    }
    return "// " + item.Name + " represents ..."
}

// ApplyDocs is permanently disabled in the server review pipeline to prevent unisolated
// host file mutations (D-13). Documentation generation is strictly report-only via PR comments.
func ApplyDocs(items []UndocumentedItem) error {
	return ErrApplyDocsDisabled
}
