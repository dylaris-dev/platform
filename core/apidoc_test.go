package main

import (
	"os"
	"strings"
	"testing"

	"dylaris-core/apidoc"
	"dylaris-core/authz"

	"github.com/gorilla/mux"
)

type walkedRoute struct {
	methods []string
	path    string
}

func (w walkedRoute) keys() []string {
	if len(w.methods) == 0 {
		return []string{"(any) " + w.path}
	}
	out := make([]string, 0, len(w.methods))
	for _, m := range w.methods {
		out = append(out, m+" "+w.path)
	}
	return out
}

// TestAPIDocCoversEveryRegisteredRoute is the guard that matters: the generator
// reads routes.go as text, so a registration written in a shape it does not
// recognise would simply vanish from the reference with nothing failing. This
// asks the built router what it actually serves and demands the two agree.
//
// The only permitted difference is a subrouter's own mount point - mux reports
// "/api" as a route because a subrouter is attached as one, but it serves
// nothing itself, and it is the nil handler that says so. A
// path test would not do: "/api/tabproxy/{token}" is a real route that also
// carries no methods and also has a longer sibling below it.
func TestAPIDocCoversEveryRegisteredRoute(t *testing.T) {
	var walked []walkedRoute
	if err := stubRouter(t).Walk(func(rt *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tpl, err := rt.GetPathTemplate()
		if err != nil || rt.GetHandler() == nil {
			return nil
		}
		methods, _ := rt.GetMethods()
		walked = append(walked, walkedRoute{methods: methods, path: tpl})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	routes, err := apidoc.Parse("routes.go", []string{"handlers"})
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, r := range routes {
		for _, k := range (walkedRoute{methods: r.Methods, path: r.Path}).keys() {
			documented[k] = true
		}
	}

	served := map[string]bool{}
	for _, w := range walked {
		for _, k := range w.keys() {
			served[k] = true
			if !documented[k] {
				t.Errorf("route %q is served but missing from the reference: the generator did not recognise its registration", k)
			}
		}
	}

	for k := range documented {
		if !served[k] {
			t.Errorf("route %q is in the reference but the router does not serve it", k)
		}
	}
}

// TestEveryRouteCapabilityIsInTheCatalog closes a gap the coverage tests leave
// open. TestRequiredCapsIntegrity validates the values in the requiredCaps map,
// but that map holds one REPRESENTATIVE capability per path template - the
// per-method capability that actually gates a route is the string passed to
// RequireCap at the registration, and nothing checked those.
//
// A typo there compiles and boots. RequireCap looks the id up per request and
// answers 500 "unknown capability" when it is unknown, and it does so BEFORE
// the admin short-circuit, so the route is dead for everyone including an
// admin, and only once someone calls it.
func TestEveryRouteCapabilityIsInTheCatalog(t *testing.T) {
	routes, err := apidoc.Parse("routes.go", nil)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, r := range routes {
		if r.Cap == "" {
			continue
		}
		checked++
		if !authz.Has(r.Cap) {
			t.Errorf("%v %s gates on %q, which is not a catalog capability: every request answers 500",
				r.Methods, r.Path, r.Cap)
		}
	}
	if checked == 0 {
		t.Fatal("no capabilities found; the parser is out of step with the source")
	}
}

// TestAPIDocIsCurrent keeps the checked-in document honest. It fails on any
// change to a route, a capability or a handler doc comment that was not
// followed by a regeneration.
func TestAPIDocIsCurrent(t *testing.T) {
	want, err := apidoc.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(apidoc.DocPath)
	if err != nil {
		t.Fatalf("%v (run: go run ./cmd/apidocs)", err)
	}
	got := apidoc.Normalize(string(raw))
	if got == want {
		return
	}

	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := "", ""
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("%s is out of date, run: go run ./cmd/apidocs\n"+
				"first difference at line %d\n  on disk:   %s\n  generated: %s", apidoc.DocPath, i+1, g, w)
		}
	}
}

// TestAPIJSONIsCurrent does the same for the machine-readable twin. Both are
// generated from one parse, so a route added without regenerating would leave
// dylaris.com/api-docs describing a router that no longer exists - and unlike the
// markdown, nobody reads that file by eye and notices.
func TestAPIJSONIsCurrent(t *testing.T) {
	want, err := apidoc.GenerateJSON(".")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(apidoc.JSONPath)
	if err != nil {
		t.Fatalf("%v (run: go run ./cmd/apidocs)", err)
	}
	if apidoc.Normalize(string(raw)) != want {
		t.Fatalf("%s is out of date, run: go run ./cmd/apidocs", apidoc.JSONPath)
	}
}
