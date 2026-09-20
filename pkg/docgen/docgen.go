package docgen

import (
    "path/filepath"
    "go/ast"
    "go/parser"
    "go/token"
    "os"
    "strings"
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

// ApplyDocs inserts generated docs into the source files. It reads the file content, inserts the comment before the declaration, and writes back.
func ApplyDocs(items []UndocumentedItem) error {
    // group by file for efficiency
    fileMap := map[string][]UndocumentedItem{}
    for _, it := range items {
        fileMap[it.File] = append(fileMap[it.File], it)
    }
    for file, its := range fileMap {
        data, err := os.ReadFile(file)
        if err != nil {
            return err
        }
        src := string(data)
        // naive line-based insertion; assumes each decl starts at line start with its name.
        lines := strings.Split(src, "\n")
        // sort items by appearance order descending to avoid offset shifts
        // We'll just loop and insert when matching line contains the name.
        for i := len(lines) - 1; i >= 0; i-- {
            line := lines[i]
            for _, it := range its {
                // simple match for func or type name
                if strings.Contains(line, it.Name) && (strings.HasPrefix(strings.TrimSpace(line), "func ") || strings.HasPrefix(strings.TrimSpace(line), "type ")) {
                    // insert doc above this line
                    doc := GenerateGoDoc(it)
                    lines = append(lines[:i], append([]string{doc}, lines[i:]...)...)
                    break
                }
            }
        }
        newSrc := strings.Join(lines, "\n")
        if err := os.WriteFile(file, []byte(newSrc), 0644); err != nil {
            return err
        }
    }
    return nil
}
