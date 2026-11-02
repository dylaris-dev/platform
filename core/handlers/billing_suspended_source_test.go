package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"dylaris-core/handlers/handlertest"
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
		"startBackupRun":  "the SCHEDULE keeps protecting a suspended tenant's data for the retention window; its on-demand caller TriggerJob carries the guard",
	}

	// Every package under handlers/, at any depth, and services/: a dispatch
	// that moved into a subpackage or behind a service is still a dispatch.
	root := handlertest.HandlersRoot(t)
	files := handlertest.SourceFiles(t, root)
	files = append(files, handlertest.SourceFiles(t, filepath.Join(root, "..", "services"))...)
	fset := token.NewFileSet()
	checked, guarded := 0, 0
	for _, f := range files {
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
					// the guard is reached through whatever package holds it
					// (handlers.RefuseIfSuspended today); the set of names is
					// what stays explicit
					if suspensionGuards[fun.Sel.Name] {
						guards = true
					}
				case *ast.Ident:
					if suspensionGuards[fun.Name] {
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
		t.Fatal("no function in this package or its subpackages sends a node command any more - has the dispatch moved? The guard has to move with it")
	}
	if guarded == 0 {
		t.Fatal("not one dispatching function carries the guard, which cannot be right")
	}
	// Floors, measured: a move that hides some dispatch sites from this walk
	// (but not all) would otherwise still pass. A change that really adds or
	// removes a dispatching function updates the numbers in the same commit.
	const wantChecked, wantGuarded = 16, 13
	if checked != wantChecked || guarded != wantGuarded {
		t.Errorf("saw %d dispatching functions, %d guarded; the pin expects %d and %d. "+
			"If dispatch moved somewhere this walk does not reach, the guard has to move with it; "+
			"if a dispatching function was really added or removed, update the floor here",
			checked, guarded, wantChecked, wantGuarded)
	}
}

var suspensionGuards = map[string]bool{
	"refuseIfSuspended": true, "suspendedForNonPayment": true, "ownerRefusal": true,
	"RefuseIfSuspended": true, "SuspendedForNonPayment": true, "OwnerRefusal": true,
}

// isQueueReceiver reports whether the expression is something.Queue - the
// selector the queue is always reached through in this package.
func isQueueReceiver(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Queue"
}
