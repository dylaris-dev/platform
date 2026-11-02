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

// TestNoAppStateFieldIsSetAfterItIsCopied pins the ordering the per-package
// Deps values depend on. A handler used to hold *AppState and read a field on
// every request, so main.go could assign SuspendGrace or Admission after the
// router was built and every handler still saw it. A Deps value copies the
// field when routes.go builds it; an assignment after that copy leaves the
// handler with the zero value - for SuspendGrace a grace of zero, for a "nil =
// off" pointer a feature silently off.
//
// The rule: in main(), no appState field is assigned after buildAPIRouter is
// called; in both functions, no field is assigned after its first read. main
// hands fields to services before the router too (the storage migration
// resolver copies CoreStorage), and a read before the assignment is a nil.
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

// TestEveryDepsLiteralFillsEveryField: a Deps field routes.go forgets to copy
// is not a compile error, it is a nil - and for most of these fields nil means
// "feature off" (no warp peers to drop, no gateway to tear down, no cache), so
// the handler would quietly skip work instead of failing. Every `<pkg>.Deps{`
// literal in routes.go must name every field of that package's Deps struct.
func TestEveryDepsLiteralFillsEveryField(t *testing.T) {
	fset := token.NewFileSet()
	routes, err := parser.ParseFile(fset, "routes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	ast.Inspect(routes, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Deps" {
			return true
		}
		pkg := sel.X.(*ast.Ident).Name
		seen++
		set := map[string]bool{}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				set[kv.Key.(*ast.Ident).Name] = true
			}
		}
		for _, f := range depsFields(t, fset, filepath.Join("handlers", pkg)) {
			if !set[f] {
				t.Errorf("%s: %s.Deps literal leaves %s unset (zero/nil)", fset.Position(lit.Pos()), pkg, f)
			}
		}
		return true
	})
	if seen == 0 {
		t.Fatal("no Deps literal found in routes.go - the walk is out of step with the source")
	}
}

func depsFields(t *testing.T, fset *token.FileSet, dir string) []string {
	t.Helper()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, p := range pkgs {
		for _, f := range p.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Deps" {
					return true
				}
				for _, fl := range ts.Type.(*ast.StructType).Fields.List {
					for _, nm := range fl.Names {
						fields = append(fields, nm.Name)
					}
				}
				return false
			})
		}
	}
	if len(fields) == 0 {
		t.Fatalf("%s: no Deps struct", dir)
	}
	return fields
}
