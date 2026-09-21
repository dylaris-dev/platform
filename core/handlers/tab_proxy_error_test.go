package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A proxied tab is served on its own content host, and a PUBLIC one is served
// to anyone holding the link. Measured on production: the visitor of a public
// share link, on a tab whose server happened to be stopped, was handed the
// node's own words -
// "mc_3acd112c-... has no network IP (is it running?)" - which told a stranger
// the server's UUID, its container name and whether it was running.
func TestProxyErrorDoesNotRepeatWhatTheNodeSaid(t *testing.T) {
	tab := &proxyTab{ID: 4, ServerUUID: "3acd112c-d3b3-4cca-a74b-91c8ae5091e8", NodeID: 7}
	rec := httptest.NewRecorder()

	proxyUpstreamError(rec, tab, http.StatusBadGateway,
		"mc_3acd112c-d3b3-4cca-a74b-91c8ae5091e8 has no network IP (is it running?)")

	body := rec.Body.String()
	for _, leak := range []string{"3acd112c", "mc_", "network IP", "is it running"} {
		if strings.Contains(body, leak) {
			t.Errorf("the answer still carries %q: %s", leak, body)
		}
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want the upstream's 502 - the code is useful and says nothing private", rec.Code)
	}
	if strings.TrimSpace(body) == "" {
		t.Error("a blank page tells the visitor nothing at all")
	}
}

// An upstream that answers something that is not an HTTP status must not become
// one: WriteHeader panics outside 100-999, and a 200 would make a failure look
// like a page.
func TestProxyErrorClampsAnImpossibleStatus(t *testing.T) {
	for _, code := range []int{0, -1, 200, 302, 1000, 99999} {
		rec := httptest.NewRecorder()
		proxyUpstreamError(rec, nil, code, "whatever")
		if rec.Code != http.StatusBadGateway {
			t.Errorf("upstream code %d became %d, want it clamped to 502", code, rec.Code)
		}
	}
}

// A code the upstream legitimately produced is kept: a 404 from the proxied app
// has to reach the browser as a 404.
func TestProxyErrorKeepsARealUpstreamStatus(t *testing.T) {
	for _, code := range []int{400, 404, 403, 500, 503} {
		rec := httptest.NewRecorder()
		proxyUpstreamError(rec, nil, code, "whatever")
		if rec.Code != code {
			t.Errorf("upstream code %d became %d", code, rec.Code)
		}
	}
}
