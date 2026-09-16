package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Two single lines carry the overlay teardown into the two paths that remove an
// account. Nothing else fails if either goes: the service keeps its tests, the
// handler keeps its tests, and a deleted tenant's WireGuard peers quietly stay
// on the leader with no row left that can name them.
//
// That is the shape the BYON release before this one shipped - a helper that was
// correct and a caller that stopped calling it - so both lines get a guard.

// The API half, through the REAL router builder rather than a stand-in.
func TestBuildAPIRouterWiresTheOverlayTeardown(t *testing.T) {
	// The concrete store, because the wiring is deliberately skipped when the
	// assertion in buildAPIRouter does not find one: a nil *PostgresStore inside
	// the warp service is not nil to a nil check, and the account delete would
	// call straight into it.
	appState := &handlers.AppState{Store: &store.PostgresStore{}}
	appState.FeatureFlags = services.NewFeatureFlags(appState.Store)
	appState.Authz = authz.NewResolver(appState.Store)
	authHandler := handlers.NewAuthHandler(appState, testJWTSecret)

	buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: testJWTSecret})

	if appState.WarpPeers == nil {
		t.Fatal("AppState.WarpPeers is unset, so deleting an account leaves its WireGuard peers on the leader forever")
	}
}

// The background half. The sweep that removes accounts in practice lives in
// main(), which no test can call, so this reads the source and names the SITE -
// the call, on that receiver - rather than grepping the file for a word.
func TestMainWiresTheOverlayTeardownIntoTheAutoDeleteSweep(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetWarpPeers" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if ok && strings.Contains(strings.ToLower(recv.Name), "autodelete") {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("main() never calls SetWarpPeers on the auto-delete sweep; its DEFAULT mode keeps the user row, so nothing cascades and the tenant's machine re-enrols into the overlay on its own timer")
	}
}
