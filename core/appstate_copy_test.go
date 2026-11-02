package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestNoAppStateFieldIsSetAfterItIsCopied pins the ordering the per-package
// Deps values depend on. A handler used to hold *AppState and read a field on
// every request, so main.go could assign SuspendGrace or Admission after the
// router was built and every handler still saw it. A Deps value copies the
// field when routes.go builds it; an assignment after that copy leaves the
// handler with the zero value - for SuspendGrace a grace of zero, for a "nil =
// off" pointer a feature silently off.
//
// The rule: in main(), no appState field is assigned after buildAPIRouter is
// called; inside buildAPIRouter, no field is assigned after its first read.
func TestNoAppStateFieldIsSetAfterItIsCopied(t *testing.T) {
	fset := token.NewFileSet()
	for _, c := range []struct{ file, fn string }{{"main.go", "main"}, {"routes.go", "buildAPIRouter"}} {
		f, err := parser.ParseFile(fset, c.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == c.fn {
				body = fd.Body
			}
		}
		if body == nil {
			t.Fatalf("%s: no func %s", c.file, c.fn)
		}
		routerBuilt := token.NoPos
		firstRead := map[string]token.Pos{}
		assigned := map[*ast.SelectorExpr]bool{}
		type write struct {
			field string
			pos   token.Pos
		}
		var writes []write
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && isAppState(sel.X) {
						assigned[sel] = true
						writes = append(writes, write{sel.Sel.Name, sel.Pos()})
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "buildAPIRouter" && routerBuilt == token.NoPos {
					routerBuilt = x.Pos()
				}
			case *ast.SelectorExpr:
				if isAppState(x.X) && !assigned[x] {
					if p, seen := firstRead[x.Sel.Name]; !seen || x.Pos() < p {
						firstRead[x.Sel.Name] = x.Pos()
					}
				}
			}
			return true
		})
		for _, w := range writes {
			if c.fn == "main" {
				if routerBuilt == token.NoPos {
					t.Fatalf("main.go: buildAPIRouter call not found")
				}
				if w.pos > routerBuilt {
					t.Errorf("%s: appState.%s is assigned after buildAPIRouter copied the state into Deps", fset.Position(w.pos), w.field)
				}
				continue
			}
			if p, seen := firstRead[w.field]; seen && w.pos > p {
				t.Errorf("%s: appState.%s is assigned after %s already read it", fset.Position(w.pos), w.field, fset.Position(p))
			}
		}
		if len(writes) == 0 && c.fn == "main" {
			t.Fatalf("main.go: no appState field assignment seen - the walk is out of step with the source")
		}
	}
}

func isAppState(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "appState"
}
