package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// The second and third patches this repo carries on Wails, held the way the
// BC3 one is (see vendor_bc3_patch_test.go for why through the syntax tree):
// the vendored tree is its own module, outside the CI matrix, and its package
// is internal, so there is no seam to call.
//
// The bridge answered any page the window held. Nothing keeps the window on
// the app's origin - a redirect the panel proxy passes through, or a file
// dropped onto the window, navigates it - and the page found there could call
// every bound method with the session the Go side holds. And every permission
// request was answered Allow: camera, microphone, location, clipboard read.

func windowsFrontend(t *testing.T) *ast.File {
	t.Helper()
	path := filepath.Join("third_party", "wails", "v2", "internal", "frontend", "desktop", "windows", "frontend.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

// firstStmtGuardsOrigin reports whether the function's first statement is
// `if !f.fromAppOrigin() { return }`.
func firstStmtGuardsOrigin(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	ifs, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || ifs.Init != nil {
		return false
	}
	not, ok := ifs.Cond.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return false
	}
	call, ok := not.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "fromAppOrigin" {
		return false
	}
	if len(ifs.Body.List) != 1 {
		return false
	}
	_, isReturn := ifs.Body.List[0].(*ast.ReturnStmt)
	return isReturn
}

func TestTheBridgeAnswersOnlyTheAppsOwnPage(t *testing.T) {
	file := windowsFrontend(t)
	for _, name := range []string{"processMessage", "processMessageWithAdditionalObjects"} {
		fn := findFunc(file, name)
		if fn == nil {
			t.Fatalf("%s is gone from the vendored frontend", name)
		}
		if !firstStmtGuardsOrigin(fn) {
			t.Errorf("%s acts on a message before checking that the page is the app's own; a foreign page in the window reaches every bound method", name)
		}
	}
}

func TestTheWebViewGrantsNoPermissionUnasked(t *testing.T) {
	file := windowsFrontend(t)
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetGlobalPermission" || len(call.Args) != 1 {
			return true
		}
		found = true
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok || arg.Sel.Name != "CoreWebView2PermissionStateDeny" {
			t.Errorf("SetGlobalPermission is not Deny: every page in the window gets camera, microphone, location and clipboard read without a prompt")
		}
		return true
	})
	if !found {
		t.Fatal("SetGlobalPermission is gone; the default would prompt, which is not what was decided here either")
	}
}
