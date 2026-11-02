package main

import (
	"testing"

	"dylaris-core/apidoc"
	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/handlers/auth"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// stubRouter builds the real router with a nil-ish AppState. Handler constructors
// only store what they are given, so this is safe; the router is walked, never served.
func stubRouter(t *testing.T) *mux.Router {
	t.Helper()
	appState := &handlers.AppState{
		FeatureFlags: services.NewFeatureFlags(nil),
		Authz:        authz.NewResolver(nil),
	}
	authHandler := auth.NewAuthHandler(authDepsOf(appState), "test-secret")
	root, _ := buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: "test-secret"})
	return root
}

func TestRouteCoverage_PermissiveRealRouterIsGreen(t *testing.T) {
	violations, err := authz.RouteCoverageViolations(stubRouter(t), requiredCaps, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("permissive mode must be green, got %v", violations)
	}
}

func TestRequiredCapsIntegrity(t *testing.T) {
	tmpls := map[string]bool{}
	_ = stubRouter(t).Walk(func(rt *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		if tpl, err := rt.GetPathTemplate(); err == nil {
			tmpls[tpl] = true
		}
		return nil
	})
	for tpl, capID := range requiredCaps {
		if !authz.Has(capID) {
			t.Errorf("requiredCaps[%q]=%q is not a catalog capability", tpl, capID)
		}
		if !tmpls[tpl] {
			t.Errorf("requiredCaps key %q matches no registered route template", tpl)
		}
	}
}

// TestNoClassificationOutlivesItsRoute is the reverse direction of
// TestEveryRouteIsClassified, and the half that was missing.
//
// requiredCaps was already checked both ways: a key matching no route fails
// above. ExemptRoutes and InHandlerAuthzRoutes were checked in one direction
// only, so an entry survived the deletion of the route it described. That is
// not cosmetic - both maps are security ALLOW-LISTS. A leftover entry is a
// pre-approved hole: whoever later registers a route at that same template
// gets it exempted from the capability gate without writing anything down,
// and TestRouteCoverage_StrictRealRouter stays green while it happens.
//
// Found with three /api/tabproxy/{token}... entries still listed after the
// path-mode tab data plane was replaced by per-tab hosts.
func TestNoClassificationOutlivesItsRoute(t *testing.T) {
	registered := map[string]bool{}
	_ = stubRouter(t).Walk(func(rt *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		if tpl, err := rt.GetPathTemplate(); err == nil {
			registered[tpl] = true
		}
		return nil
	})
	for _, m := range []struct {
		name string
		set  map[string]bool
	}{
		{"authz.ExemptRoutes", authz.ExemptRoutes},
		{"authz.InHandlerAuthzRoutes", authz.InHandlerAuthzRoutes},
	} {
		for tpl, on := range m.set {
			if !on || registered[tpl] {
				continue
			}
			t.Errorf("%s[%q] matches no registered route - delete it, or it silently exempts whatever is registered there next", m.name, tpl)
		}
	}
}

// TestEveryRouteIsClassified is the Phase 4 Task 22 pre-flight for the strict
// coverage flip (Task 23): every registered route template must land in
// EXACTLY ONE of requiredCaps, authz.ExemptRoutes, or authz.InHandlerAuthzRoutes.
// A route that is none of these is a gap that the strict flip would leave
// ungated, so this fails loudly and prints every unclassified template.
func TestEveryRouteIsClassified(t *testing.T) {
	var unclassified []string
	_ = stubRouter(t).Walk(func(rt *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, err := rt.GetPathTemplate()
		if err != nil {
			return nil
		}
		if authz.ExemptRoutes[tpl] || authz.InHandlerAuthzRoutes[tpl] {
			return nil
		}
		if _, ok := requiredCaps[tpl]; ok {
			return nil
		}
		unclassified = append(unclassified, tpl)
		return nil
	})
	if len(unclassified) != 0 {
		t.Fatalf("unclassified routes (add to requiredCaps/ExemptRoutes/InHandlerAuthzRoutes): %v", unclassified)
	}
}

// TestBucketsAreDisjoint guards the "EXACTLY one bucket" half of the coverage
// invariant: no route template may appear in more than one of requiredCaps /
// ExemptRoutes / InHandlerAuthzRoutes. Without this, a template landing in both
// requiredCaps and ExemptRoutes would be silently treated as exempt (ExemptRoutes
// short-circuits first in RouteCoverageViolations), quietly un-gating a route that
// is supposed to carry a capability. Together with TestEveryRouteIsClassified
// (at least one bucket) this pins exactly-one.
func TestBucketsAreDisjoint(t *testing.T) {
	for tpl := range requiredCaps {
		if authz.ExemptRoutes[tpl] {
			t.Errorf("template %q is in BOTH requiredCaps and ExemptRoutes", tpl)
		}
		if authz.InHandlerAuthzRoutes[tpl] {
			t.Errorf("template %q is in BOTH requiredCaps and InHandlerAuthzRoutes", tpl)
		}
	}
	for tpl := range authz.ExemptRoutes {
		if authz.InHandlerAuthzRoutes[tpl] {
			t.Errorf("template %q is in BOTH ExemptRoutes and InHandlerAuthzRoutes", tpl)
		}
	}
}

// TestRouteCoverage_StrictRealRouter is the standing "nothing is un-gateable"
// guarantee (Phase 4 Task 23): every non-exempt, non-in-handler route must declare
// a capability in requiredCaps, or the build fails. This is the enforcement
// cut-over's completion - from here, adding a new /api route without a cap (or an
// explicit exempt/in-handler classification) breaks the test suite.
func TestRouteCoverage_StrictRealRouter(t *testing.T) {
	violations, err := authz.RouteCoverageViolations(stubRouter(t), requiredCaps, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("STRICT coverage: %d un-gated route(s): %v", len(violations), violations)
	}
}

// TestPrintRouteTemplates: `go test -run TestPrintRouteTemplates -v` prints every
// template + methods so later batches copy exact requiredCaps keys.
func TestPrintRouteTemplates(t *testing.T) {
	if testing.Short() {
		t.Skip("printer utility")
	}
	_ = stubRouter(t).Walk(func(rt *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, _ := rt.GetPathTemplate()
		methods, _ := rt.GetMethods()
		t.Logf("%v %s", methods, tpl)
		return nil
	})
}

// uncappedMethods lists, per METHOD and template, the registrations under a
// requiredCaps template that deliberately carry no RequireCap. Each one is a
// read whose handler serves every authenticated caller (or filters for them);
// the write beside it on the same template is capped. A new entry here is a
// security decision and needs the same justification as an ExemptRoutes one.
var uncappedMethods = map[string]string{
	"GET /api/modules":               "navbar for every user; admin-only modules filtered in-handler by role",
	"GET /api/nodes":                 "GetNodes filters: admin sees all, a BYON tenant only its own nodes",
	"GET /api/settings/features":     "feature flags drive UI for every user; no secret fields",
	"GET /api/settings/beam":         "relay address + download link the Files tab needs",
	"GET /api/settings/routing-mode": "drives the file-access-mode choice in the UI",
}

// TestEveryMethodCarriesItsCapability closes the hole the template-keyed
// coverage leaves: requiredCaps is satisfied as soon as ONE method of a
// template carries a capability, so dropping RequireCap from
// PUT /api/admin/settings/features stayed green because its GET sibling
// carried settings.read. Here every method+template pair under a requiredCaps
// template must carry a catalog capability or be listed in uncappedMethods,
// and the template's declared capability must be carried by one of them.
func TestEveryMethodCarriesItsCapability(t *testing.T) {
	routes, err := apidoc.Parse("routes.go", []string{"handlers"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	carried := map[string]bool{}
	for _, r := range routes {
		declared, ok := requiredCaps[r.Path]
		if !ok {
			continue
		}
		for _, c := range r.Caps {
			if !authz.Has(c) {
				t.Errorf("%v %s carries %q, not a catalog capability", r.Methods, r.Path, c)
			}
			if c == declared {
				carried[r.Path] = true
			}
		}
		for _, k := range (walkedRoute{methods: r.Methods, path: r.Path}).keys() {
			seen[k] = true
			_, exempt := uncappedMethods[k]
			switch {
			case len(r.Caps) == 0 && !exempt:
				t.Errorf("%s carries no RequireCap although its template is in requiredCaps (%s); gate it, or list it in uncappedMethods with the reason", k, declared)
			case len(r.Caps) != 0 && exempt:
				t.Errorf("uncappedMethods[%q] is stale: the route carries %v now", k, r.Caps)
			}
		}
	}
	for tpl, c := range requiredCaps {
		if !carried[tpl] {
			t.Errorf("requiredCaps[%q]=%q is carried by none of its methods", tpl, c)
		}
	}
	for k := range uncappedMethods {
		if !seen[k] {
			t.Errorf("uncappedMethods[%q] matches no registration under a requiredCaps template - delete it", k)
		}
	}
}
