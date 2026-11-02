package main

import (
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/auth"
	"dylaris-core/handlers/reqctx"
)

// tabTickets is the only bridge between AuthHandler and the tab proxy. Its
// Parse must keep AuthHandler's purpose check (a session token is not a
// ticket) and carry the three claims the proxy decides on.
func TestTabTicketsBridgeKeepsThePurposeCheck(t *testing.T) {
	ah := auth.NewAuthHandler(auth.Deps{}, "tab-tickets-bridge-secret")
	tk := tabTickets(ah)

	ticket, err := tk.IssueTabProxyTicket("", "owner", false, 7, 9, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tk.ParseTabProxyTicket(ticket)
	if err != nil || got.ServerID != 7 || got.TabID != 9 || !got.ReadOnly {
		t.Fatalf("ticket parsed as %+v, %v", got, err)
	}

	session, err := ah.IssueToken("owner", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tk.ParseTabProxyTicket(session); err == nil {
		t.Fatalf("a session token parsed as a tab ticket: %+v", got)
	}

	r := httptest.NewRequest("GET", "/", nil)
	if id := tk.SessionID(r); id != "" {
		t.Fatalf("no session, SessionID = %q", id)
	}
	c := &auth.Claims{}
	c.ID = "sess-1"
	r = r.WithContext(reqctx.WithSessionClaims(r.Context(), c))
	if id := tk.SessionID(r); id != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", id)
	}
}
