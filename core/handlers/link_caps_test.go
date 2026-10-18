package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The heartbeat stores what the link announced, built from the whitelist, and
// keeps writing "1" for an old link. The edge reads this value to decide
// whether a link understands the client-address stream, so a foreign string
// reaching it would be a claim no link made.
func TestLinkPresenceValue(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"new link", `{"caps":["client-addr"]}`, `{"caps":["client-addr"]}`},
		{"old link sends {}", `{}`, "1"},
		{"empty body", ``, "1"},
		{"not json", `caps=client-addr`, "1"},
		{"caps of the wrong type", `{"caps":"client-addr"}`, "1"},
		{"only unknown caps", `{"caps":["evil","<script>"]}`, "1"},
		{"unknown and duplicate caps dropped", `{"caps":["evil","client-addr","client-addr"]}`, `{"caps":["client-addr"]}`},
		{"other fields ignored", `{"caps":["client-addr"],"x":{"y":1}}`, `{"caps":["client-addr"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkPresenceValue(strings.NewReader(tc.body)); got != tc.want {
				t.Errorf("linkPresenceValue(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// End to end through the handler: the caps reach online_link:<token> with the
// same 15 s TTL, and a later {} heartbeat (a link rolled back) takes them away.
func TestLinkHeartbeatStoresAnnouncedCaps(t *testing.T) {
	h, _, key, token := routeOnlyKit(t)
	rdb := h.state.Redis
	ctx := context.Background()

	rec := callAs(h.LinkHeartbeat, http.MethodPost, "/api/warp/link/heartbeat", `{"caps":["client-addr"]}`, key)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := rdb.Get(ctx, "online_link:"+token).Result(); v != `{"caps":["client-addr"]}` {
		t.Errorf("online_link:<token> = %q, want the caps JSON", v)
	}
	if d := ttl(t, rdb, "online_link:"+token); d != 15*time.Second {
		t.Errorf("online_link:<token> TTL %v, want 15s", d)
	}

	rec = callAs(h.LinkHeartbeat, http.MethodPost, "/api/warp/link/heartbeat", `{}`, key)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := rdb.Get(ctx, "online_link:"+token).Result(); v != "1" {
		t.Errorf("online_link:<token> after {} = %q, want 1", v)
	}
}
