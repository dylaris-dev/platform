package tickets

import "testing"

// server_uuid only ever covered servers, so a customer whose BYON machine or
// protected address was the problem had nowhere to say so and support had to
// work it out from the prose.
//
// The rule that matters: a bad subject NEVER blocks the ticket. It is context
// for whoever reads it, and refusing to file a support request over it would
// turn a wrong dropdown value into a customer who cannot ask for help.
func TestNormalizeTicketSubject(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		ref      string
		wantKind string
		wantRef  string
	}{
		{name: "nothing specific", kind: "", ref: "", wantKind: "", wantRef: ""},
		{
			// The id lives in ServerUUID. Recording it here too would create
			// two places that can disagree about which server it is.
			name: "a server carries no ref",
			kind: "server", ref: "some-uuid",
			wantKind: "server", wantRef: "",
		},
		{
			name: "a node keeps its id",
			kind: "node", ref: "home-desktop",
			wantKind: "node", wantRef: "home-desktop",
		},
		{
			name: "a route keeps its domain",
			kind: "route", ref: "mc.example.com",
			wantKind: "route", wantRef: "mc.example.com",
		},
		{
			name: "case and padding are normalized",
			kind: "  Node  ", ref: "  home-desktop  ",
			wantKind: "node", wantRef: "home-desktop",
		},
		{
			// A node or route with nothing naming it says less than nothing.
			name: "a node without a ref is nothing specific",
			kind: "node", ref: "   ",
			wantKind: "", wantRef: "",
		},
		{
			// The column is VARCHAR(128); an over-long ref would fail the
			// INSERT and take the whole ticket down with it.
			name: "an over-long ref is dropped, not stored",
			kind: "route", ref: string(make([]byte, 129)),
			wantKind: "", wantRef: "",
		},
		{
			name: "an unknown kind is nothing specific",
			kind: "database", ref: "prod",
			wantKind: "", wantRef: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ref := normalizeTicketSubject(tt.kind, tt.ref)
			if kind != tt.wantKind || ref != tt.wantRef {
				t.Errorf("normalizeTicketSubject(%q, %q) = (%q, %q), want (%q, %q)",
					tt.kind, tt.ref, kind, ref, tt.wantKind, tt.wantRef)
			}
		})
	}
}
