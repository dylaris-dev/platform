package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Suspension for non-payment used to be enforced in ONE place, on start and
// restart. That is not where the line is: a suspended tenant called
// POST /servers/{id}/setup on production and the node installed and booted the
// server, so the customer undid their own cutoff with one call.
//
// The real rule is "a cut-off tenant may not make a node work for them", and a
// node is made to work by exactly one thing in this package - a command on the
// queue. So this test finds every function that sends one and requires the
// guard, rather than trusting that whoever adds the next endpoint remembers a
// rule written in a comment somewhere else.
//
// It walks the AST rather than matching text because the question is per
// FUNCTION: a file can hold a guarded handler and an unguarded one.
func TestEveryHandlerThatMakesANodeWorkChecksSuspension(t *testing.T) {
	// Functions that send a command and must NOT be guarded, each with the
	// reason. A cutoff is not a lockout: it stops service, it does not trap
	// the tenant or their data.
	exempt := map[string]string{
		"DeleteServer":    "deleting frees resources and is how a cut-off tenant leaves; blocking it would trap them",
		"DeleteSubServer": "same as DeleteServer, one directory down",
		"CreateServer":    "creation is gated by entitlement and placement, and an admin creating a server for a tenant is not the tenant asking for service",
		"startBackupRun":  "the SCHEDULE keeps protecting a suspended tenant's data for the retention window; its on-demand caller TriggerJob carries the guard",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	checked, guarded := 0, 0
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
			sends, guards := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if strings.HasPrefix(fun.Sel.Name, "Send") && isQueueReceiver(fun.X) {
						sends = true
					}
				case *ast.Ident:
					if fun.Name == "refuseIfSuspended" || fun.Name == "suspendedForNonPayment" {
						guards = true
					}
				}
				return true
			})
			if !sends {
				continue
			}
			checked++
			if reason, ok := exempt[fn.Name.Name]; ok {
				if guards {
					t.Errorf("%s: %s is on the exempt list (%s) but guards anyway - decide which is true",
						f, fn.Name.Name, reason)
				}
				continue
			}
			if !guards {
				t.Errorf("%s: %s sends a command to a node without checking suspension. "+
					"A tenant who is cut off for non-payment can reach it and make their node work. "+
					"Add refuseIfSuspended, or add it to the exempt map above with the reason it must stay open.",
					f, fn.Name.Name)
				continue
			}
			guarded++
		}
	}
	// If the dispatch moves behind a helper this test stops seeing anything,
	// and silence would read as a pass.
	if checked == 0 {
		t.Fatal("no function in this package sends a node command any more - has the dispatch moved? The guard has to move with it")
	}
	if guarded == 0 {
		t.Fatal("not one dispatching function carries the guard, which cannot be right")
	}
}

// isQueueReceiver reports whether the expression is something.Queue - the
// selector the queue is always reached through in this package.
func isQueueReceiver(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Queue"
}
