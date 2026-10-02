package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "dylaris-proto/node"
)

// A page on another tab's host can set a cookie of the same name for the
// shared parent domain. Only the first one was read, so it locked the viewer
// out of every other tab.
func TestATossedTabCookieDoesNotLockOutTheRealOne(t *testing.T) {
	h, ah := newHostGateHandler(t, true, false)
	foreign, _ := ah.IssueTabProxyTicket("owner", false, 1, 99, false)
	own, _ := ah.IssueTabProxyTicket("owner", false, 1, 2, false)
	r := httptest.NewRequest(http.MethodGet, "https://x.share.example.com/", nil)
	r.AddCookie(&http.Cookie{Name: proxyCookieName, Value: foreign})
	r.AddCookie(&http.Cookie{Name: proxyCookieName, Value: own})
	if authed, ok, _ := h.resolvePublicTicket(r, proxiedTab("private", "")); !authed || !ok {
		t.Fatalf("authed=%v access=%v; the viewer's own ticket was ignored", authed, ok)
	}
}

// Clear-Site-Data applies to the whole registrable domain: a tab's page could
// sign its viewer out of the panel.
func TestATabCannotClearThePanelsSiteData(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProxyHeaders(rec, []*pb.HttpHeader{{Key: "Clear-Site-Data", Value: `"cookies"`}, {Key: "X-Kept", Value: "1"}}, false)
	if rec.Header().Get("Clear-Site-Data") != "" || rec.Header().Get("X-Kept") != "1" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

// RCON took any characters; the console has refused control characters for
// a while.
func TestAnRCONCommandWithControlCharactersIsRefused(t *testing.T) {
	h := &RconHandler{state: &AppState{}}
	resp := h.execAgainstServer(t.Context(), 1, "u", 1, rconRequest{Command: "say hi\x00\x1b[2J"})
	if resp.status != http.StatusBadRequest {
		t.Fatalf("status %d (%s), want 400", resp.status, resp.Error)
	}
}
