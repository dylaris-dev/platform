package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// The BC3 patch is the one change this repo carries on top of upstream Wails,
// and nothing was holding it.
//
// What it does: the beam window reverse-proxies the untrusted remote Panel onto
// its own origin, so page script can post "BO:<url>" through WailsInvoke and
// reach the native OS shell-open with a string it chose. Upstream guards that
// only in JavaScript, which is not a boundary when the attacker IS the page.
// The patch moves the check into the native dispatcher and hands the OS only
// web schemes.
//
// Why the assertion is made against the SOURCE rather than by calling the
// function: the patched file lives in third_party/wails/v2, which is its own Go
// module (github.com/wailsapp/wails/v2) and is not in the CI test matrix, and
// the package is under internal/, so this module cannot import it either. There
// is no seam to call. What CAN be checked is the property itself - that every
// native open in that function is reachable only through the allowed schemes -
// and that is what this does, through the syntax tree rather than a substring,
// so reformatting does not break it and a comment cannot satisfy it.
//
// A plain `go get -u` in that vendored tree would reopen the RCE class with
// nothing failing anywhere. Now this fails.
func TestBC3PatchStillGuardsTheNativeBrowserOpen(t *testing.T) {
	path := filepath.Join("third_party", "wails", "v2", "internal", "frontend", "dispatcher", "browser.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v\nThe patched dispatcher must stay where VENDOR.md says it is.", path, err)
	}

	fn := findFunc(file, "processBrowserMessage")
	if fn == nil {
		t.Fatalf("processBrowserMessage is gone from %s - the vendored tree was replaced", path)
	}

	// Every native open, with the case labels it sits under. A call outside any
	// case clause is reported with no labels, which fails below.
	var opens []([]string)
	var stack []ast.Node
	ast.Inspect(fn, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "BrowserOpenURL" {
			return true
		}
		opens = append(opens, enclosingCaseSchemes(stack))
		return true
	})

	if len(opens) == 0 {
		t.Fatal("nothing in processBrowserMessage opens a URL natively any more; either the patch removed the feature or the vendored tree changed shape")
	}

	// mailto rides along because it is the one non-http scheme a panel link
	// legitimately uses and it cannot name a local file. Anything beyond these
	// three - file, smb, a bare drive path, javascript - is the hole.
	want := []string{"http", "https", "mailto"}
	for i, schemes := range opens {
		if len(schemes) == 0 {
			t.Errorf("native open #%d is not inside a scheme case at all: page script reaches the OS shell with any string", i)
			continue
		}
		if !sameStrings(schemes, want) {
			t.Errorf("native open #%d is reachable for schemes %v, want exactly %v", i, schemes, want)
		}
	}
}

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// enclosingCaseSchemes returns the string literals of the innermost case clause
// the node sits in, or nil when it is in none (or in a default clause, which
// carries no list and is exactly as bad as no guard).
func enclosingCaseSchemes(stack []ast.Node) []string {
	for i := len(stack) - 1; i >= 0; i-- {
		clause, ok := stack[i].(*ast.CaseClause)
		if !ok {
			continue
		}
		var out []string
		for _, expr := range clause.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return nil
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return nil
			}
			out = append(out, s)
		}
		return out
	}
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
