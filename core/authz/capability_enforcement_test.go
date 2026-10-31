package authz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// capsWithoutAChokepoint is the allowlist for the reverse of what coverage.go
// checks, and every entry has to say WHY.
//
// coverage.go answers "does every ROUTE carry a capability" and is careful about
// it - ExemptRoutes, InHandlerAuthzRoutes, TestEveryRouteIsClassified. Nothing
// answered the other direction: does every declared CAPABILITY reach a
// chokepoint. It does not fail loudly. The capability appears in the panel's
// role editor, an operator ticks it to build a support role, and it confers
// nothing - while ticking it OFF withholds nothing either. Both readings are
// wrong and neither is visible.
//
// Five were found unenforced this way and removed from the catalog instead of
// being listed here: plans.delete (no plan-deletion endpoint exists at all - the
// store's DeletePlan has no caller), servers.delete ("Delete any server", with
// no DELETE under /api/admin/servers beside its enforced read/write siblings),
// and the whole staff.modpack.read/write/delete category (modpacks are gated by
// the owner-scoped modpack.* caps; there is no staff oversight path).
//
// Adding an entry here is a decision to declare a capability that does nothing.
// Wiring it up is almost always the better answer.
var capsWithoutAChokepoint = map[string]string{
	"library.read": "the Library is removed and no route exists. Kept in the catalog rather than removed because " +
		"they are ScopeOwner and ValidKeyCap / validateServerOwnerCaps accept them, so an existing API key, server " +
		"role or grant override may carry one and would fail validation on its next save.",
	"library.write":  "see library.read",
	"library.delete": "see library.read",
}

// TestEveryCapabilityReachesAChokepoint is the mirror of TestEveryRouteIsClassified.
//
// A capability is "reached" when its id appears as a string literal anywhere in
// the core module outside this package - a RequireCap on a route, a HasCap in a
// handler, an entry in the route-to-capability map. This package is excluded
// because declaring it (catalog.go) and granting it (presets.go) are not
// enforcing it, which is the entire distinction under test.
func TestEveryCapabilityReachesAChokepoint(t *testing.T) {
	refs := capabilityLiteralsInCore(t)

	// An extraction that stops matching would report every capability as
	// unenforced, which is loud, or - if it broke the other way and matched
	// everything - would pass by looking at nothing. Both are worth catching.
	if len(refs) < 50 {
		t.Fatalf("only %d string literals looked like capability ids across the core module; the extraction is broken, not the code", len(refs))
	}

	for _, c := range All() {
		if refs[c.ID] {
			if why, listed := capsWithoutAChokepoint[c.ID]; listed {
				t.Errorf("capability %q IS enforced now but is still listed in capsWithoutAChokepoint (%q); remove the entry", c.ID, why)
			}
			continue
		}
		if _, listed := capsWithoutAChokepoint[c.ID]; listed {
			continue
		}
		t.Errorf("capability %q (%s, %s) is declared in the catalog and enforced NOWHERE: "+
			"the role editor offers it, granting it confers nothing and withholding it withholds nothing. "+
			"Wire it to a chokepoint, or remove it from the catalog, or add it to capsWithoutAChokepoint with a reason.",
			c.ID, c.Scope, c.Category)
	}
}

// capabilityLiteralsInCore collects every string literal in the core module that
// could be a capability id, parsed rather than grepped so comments and doc
// examples cannot contribute.
func capabilityLiteralsInCore(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// This package declares and grants; neither is enforcement.
			if d.Name() == "authz" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file that will not parse cannot be scanned, and silently
			// skipping it is how this test would start passing for the wrong
			// reason.
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.Contains(v, ".") && !strings.ContainsAny(v, " /{}") {
				out[v] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk core module: %v (if the layout changed, this test moves with it - do not delete it)", err)
	}
	return out
}
