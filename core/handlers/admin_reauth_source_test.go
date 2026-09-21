package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The admin actions that hand out DURABLE access re-authenticate, and this
// pins the list so the next one added beside them cannot quietly skip it.
//
// Measured on production: with an admin session alone and no password, any
// account's password could be set, its second factor stripped, its address
// changed - the three takeovers - and a fresh admin account created. The
// session-kill that covers a password change reaches none of it: it ends the
// VICTIM's sessions, while the borrowed admin session is the thing asking.
//
// Named functions rather than a behavioural property, unlike the suspension
// source test next door. There is no single call that marks "this hands out
// power": creating a privileged account, assigning a panel role and stripping
// a second factor have nothing in common at the AST level. So the list is the
// decision, written once, and the test is what keeps the code matching it.
func TestAdminAccountActionsReauthenticate(t *testing.T) {
	// handler -> why it is on the list.
	want := map[string]string{
		"ResetUserPassword":         "setting somebody's password is taking their account over",
		"AdminResetTOTPHandler":     "removing the second factor leaves the password as the whole credential",
		"SetEmail":                  "the address is where a password reset goes",
		"CreateUser":                "a privileged account outlives the session that created it",
		"SetUserRoleHandler":        "admin and support are durable power",
		"SetUserPermissionsHandler": "the flags are rights of their own",
		"SetUserPanelRoleHandler":   "a panel role is the level-1 grant; panelroles.write alone is effectively full admin",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, ok := want[fn.Name.Name]; !ok {
				continue
			}
			found[fn.Name.Name] = true
			calls := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "requireAdminReauth" {
					calls = true
				}
				return true
			})
			if !calls {
				t.Errorf("%s does not re-authenticate the administrator: %s", fn.Name.Name, want[fn.Name.Name])
			}
		}
	}

	// A handler that was renamed away would otherwise pass by not being there.
	for name := range want {
		if !found[name] {
			t.Errorf("%s was not found in this package; if it moved or was renamed, move the entry with it", name)
		}
	}
}
