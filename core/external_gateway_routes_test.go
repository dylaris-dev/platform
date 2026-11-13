package main

import (
	"strings"
	"testing"

	"dylaris-core/apidoc"
)

// TestExternalGatewayRoutesGoThroughTheAdapter pins the chain of every
// /api/external/gateway route: a key route with the gateway capability for its
// verb, the owner context, and an ExternalGatewayHandler method last. The last
// part is the one that matters - it is where an admin-owned key loses its
// admin bypasses, so a route wired straight to a session handler would hand
// them back.
func TestExternalGatewayRoutesGoThroughTheAdapter(t *testing.T) {
	routes, err := apidoc.Parse("routes.go", []string{"handlers"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "/api/external/gateway/") {
			continue
		}
		n++
		want := "gateway.write"
		if len(r.Methods) == 1 && r.Methods[0] == "GET" {
			want = "gateway.read"
		}
		gates := strings.Join(r.Gates, ",")
		if r.Auth != "user API key" || r.Cap != want || !strings.Contains(gates, "ExternalOwnerRoute") ||
			!strings.HasPrefix(r.Handler, "ExternalGatewayHandler.") {
			t.Errorf("%v %s: auth=%q cap=%q gates=%q handler=%q, want user API key / %s / ExternalOwnerRoute / ExternalGatewayHandler.*",
				r.Methods, r.Path, r.Auth, r.Cap, gates, r.Handler, want)
		}
	}
	if n != 23 {
		t.Fatalf("%d external gateway routes, want 23", n)
	}
}
