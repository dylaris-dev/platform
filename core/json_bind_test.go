package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Core's Postgres connection runs with binary_parameters=yes (database/db.go):
// one round trip per statement instead of two. That option sends a []byte
// argument in BINARY format - right for bytea, and a hard error for json/jsonb
// ("unsupported jsonb version number 123"). Turning it on broke 27 DB tests,
// every one a json.Marshal result bound as it came back.
//
// So JSON is bound as a string. This test finds the shape that breaks: an
// argument of Exec/Query/QueryRow(Context) that is a json.Marshal result, a
// json.RawMessage or a []byte(...) conversion in the same function. Real bytea
// (voice uplink keys) arrives as a parameter or field and is not matched.
func TestNoJSONBoundAsBytes(t *testing.T) {
	sqlCalls := map[string]bool{"Exec": true, "ExecContext": true, "Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true}
	isByteConv := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch f := c.Fun.(type) {
		case *ast.ArrayType:
			id, ok := f.Elt.(*ast.Ident)
			return ok && id.Name == "byte"
		case *ast.SelectorExpr:
			x, ok := f.X.(*ast.Ident)
			return ok && x.Name == "json" && (f.Sel.Name == "RawMessage" || f.Sel.Name == "Marshal" || f.Sel.Name == "MarshalIndent")
		}
		return false
	}
	var bad []string
	fset := token.NewFileSet()
	for _, root := range []string{"store", "services", "handlers", "database"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				jsonVars := map[string]bool{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if as, ok := n.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && isByteConv(as.Rhs[0]) {
						if id, ok := as.Lhs[0].(*ast.Ident); ok {
							jsonVars[id.Name] = true
						}
					}
					return true
				})
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !sqlCalls[sel.Sel.Name] {
						return true
					}
					for _, a := range call.Args {
						id, isIdent := a.(*ast.Ident)
						if (isIdent && jsonVars[id.Name]) || isByteConv(a) {
							bad = append(bad, fset.Position(a.Pos()).String())
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("JSON bound as []byte (bind string(x) instead):\n  %s", strings.Join(bad, "\n  "))
	}
}
