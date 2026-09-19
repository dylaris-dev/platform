package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"

	"dylaris-pkg/protocol"

	"github.com/redis/go-redis/v9"
)

// routeOnlyKit adds a live route-only kit to the node-link fixture and returns
// its key and the tunnel token Core derives for it.
func routeOnlyKit(t *testing.T) (*WarpHandler, *nodeLinkFakeStore, store.WarpAPIKey, string) {
	t.Helper()
	h, fs, _ := newNodeLinkHandler(t)
	fs.keys["link-xyz"] = &store.WarpAPIKey{ID: 9, NodeID: "link-xyz", OwnerID: "owner-1"}
	return h, fs, *fs.keys["link-xyz"], h.state.Gateway.LinkToken("link-xyz")
}

func callAs(h http.HandlerFunc, method, path, body string, key store.WarpAPIKey) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, r.WithContext(context.WithValue(r.Context(), warpKeyCtx, key)))
	return rec
}

func ttl(t *testing.T, rdb *redis.Client, key string) time.Duration {
	t.Helper()
	d, err := rdb.TTL(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The heartbeat writes exactly the keys the edge (link:<token>, 24h) and the
// panel (online_link:<token>, 15s) read, with the TTLs the link used to write.
func TestLinkHeartbeatWritesTheKeysTheReadersRead(t *testing.T) {
	h, _, key, token := routeOnlyKit(t)
	rec := callAs(h.LinkHeartbeat, http.MethodPost, "/api/warp/link/heartbeat", "{}", key)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rdb := h.state.Redis
	if v, _ := rdb.Get(context.Background(), "link:"+token).Result(); v != "valid" {
		t.Errorf("link:<token> = %q, want valid", v)
	}
	if d := ttl(t, rdb, "link:"+token); d != 24*time.Hour {
		t.Errorf("link:<token> TTL %v, want 24h", d)
	}
	if d := ttl(t, rdb, "online_link:"+token); d != 15*time.Second {
		t.Errorf("online_link:<token> TTL %v, want 15s", d)
	}
}

// A cut-off owner's heartbeat is refused AND takes the keys down at once, so the
// edges drop the tunnel within their 30s recheck instead of at the next hourly
// enforcement pass.
func TestLinkHeartbeatOfACutOffOwnerTakesTheTunnelDown(t *testing.T) {
	h, fs, key, token := routeOnlyKit(t)
	ctx := context.Background()
	h.state.Redis.Set(ctx, "link:"+token, "valid", time.Hour)
	h.state.Redis.Set(ctx, "online_link:"+token, "1", time.Hour)
	past := time.Now().Add(-72 * time.Hour)
	fs.billing = &store.UserBilling{Status: "suspended", SuspendedAt: &past}

	rec := callAs(h.LinkHeartbeat, http.MethodPost, "/api/warp/link/heartbeat", "{}", key)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if n, _ := h.state.Redis.Exists(ctx, "link:"+token, "online_link:"+token).Result(); n != 0 {
		t.Errorf("%d key(s) survived the cut-off heartbeat", n)
	}
}

// revokeAfterReads answers the kit as live for the first n reads and as revoked
// after, which is a revoke landing while a heartbeat is in flight.
type revokeAfterReads struct {
	*nodeLinkFakeStore
	n int
}

func (s *revokeAfterReads) GetWarpAPIKeyByNodeID(id string) (*store.WarpAPIKey, error) {
	k, err := s.nodeLinkFakeStore.GetWarpAPIKeyByNodeID(id)
	if s.n--; s.n < 0 && k != nil {
		now := time.Now()
		k.RevokedAt = &now
	}
	return k, err
}

// A revoke racing a heartbeat must not leave the token valid. Revoked before
// the write: nothing is written. Revoked between the write and the re-check:
// the heartbeat deletes what it wrote.
func TestLinkHeartbeatNeverLeavesARevokedTokenValid(t *testing.T) {
	for name, liveReads := range map[string]int{"revoked before the write": 0, "revoked right after the write": 1} {
		t.Run(name, func(t *testing.T) {
			h, fs, key, token := routeOnlyKit(t)
			h.state.Store = &revokeAfterReads{nodeLinkFakeStore: fs, n: liveReads}
			rec := callAs(h.LinkHeartbeat, http.MethodPost, "/api/warp/link/heartbeat", "{}", key)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
			if n, _ := h.state.Redis.Exists(context.Background(), "link:"+token, "online_link:"+token).Result(); n != 0 {
				t.Errorf("a revoked kit's token is still valid (%d key(s))", n)
			}
		})
	}
}

// Only a live, owned link- key reaches any of the three: a node's key, an
// owner-less link key and a platform key are refused, and nothing is written.
func TestLinkAPIRefusesEveryOtherKey(t *testing.T) {
	h, fs, _, _ := routeOnlyKit(t)
	endpoints := map[string]http.HandlerFunc{"edges": h.LinkEdges, "heartbeat": h.LinkHeartbeat, "stats": h.LinkStats}
	keys := map[string]store.WarpAPIKey{
		"node key":            *fs.keys["node-abc"],
		"owner-less link key": {ID: 11, NodeID: "link-platform"},
		"platform key":        {ID: 12},
	}
	for kn, k := range keys {
		for en, fn := range endpoints {
			rec := callAs(fn, http.MethodPost, "/api/warp/link/"+en, `{"v":1}`, k)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s on %s: status %d, want 403", kn, en, rec.Code)
			}
		}
	}
	if n, _ := h.state.Redis.DBSize(context.Background()).Result(); n != 0 {
		t.Errorf("refused calls wrote %d key(s)", n)
	}
}

// The edge list is every edge in sys:edges with a public address, with the
// fingerprint it published; an expired or address-less registry is left out.
func TestLinkEdgesListsEveryDialableEdgeWithItsPin(t *testing.T) {
	h, _, key, _ := routeOnlyKit(t)
	ctx := context.Background()
	rdb := h.state.Redis
	rdb.SAdd(ctx, "sys:edges", "e1", "e2", "e3", "e4")
	rdb.Set(ctx, "edge:registry:e1", `{"edge_id":"e1","ip":"1.2.3.4","private_ip":"10.0.0.1","service_port":"25560"}`, 0)
	rdb.Set(ctx, "edge:cert:fingerprint:e1", "ff00", 0)
	rdb.Set(ctx, "edge:registry:e2", `{"edge_id":"e2","private_ip":"10.0.0.2","service_port":"25560"}`, 0)
	rdb.Set(ctx, "edge:registry:e4", `{"edge_id":"e4","ip":"5.6.7.8","service_port":"25560"}`, 0)

	rec := callAs(h.LinkEdges, http.MethodGet, "/api/warp/link/edges", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct{ Edges []linkEdge }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]linkEdge{
		"e1": {ID: "e1", Addr: "1.2.3.4:25560", Fingerprint: "ff00"},
		"e4": {ID: "e4", Addr: "5.6.7.8:25560"},
	}
	if len(got.Edges) != len(want) {
		t.Fatalf("edges %+v, want %+v", got.Edges, want)
	}
	for _, e := range got.Edges {
		if want[e.ID] != e {
			t.Errorf("edge %+v, want %+v", e, want[e.ID])
		}
	}
}

// The link does not name its own stream or the id in it: both are the kit's.
func TestLinkStatsLandInTheKitsOwnStream(t *testing.T) {
	h, _, key, _ := routeOnlyKit(t)
	rec := callAs(h.LinkStats, http.MethodPost, "/api/warp/link/stats",
		`{"v":1,"component":"edge","id":"link-someone-else","counters":{"tunnels_established":2}}`, key)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	if n, _ := h.state.Redis.Exists(ctx, "dylaris:link:link-someone-else:stats").Result(); n != 0 {
		t.Error("the link chose the stream it wrote to")
	}
	msgs, err := h.state.Redis.XRange(ctx, "dylaris:link:link-xyz:stats", "-", "+").Result()
	if err != nil || len(msgs) != 1 {
		t.Fatalf("stream: %v, %v", msgs, err)
	}
	var s protocol.GatewayStats
	if err := json.Unmarshal([]byte(msgs[0].Values["data"].(string)), &s); err != nil {
		t.Fatal(err)
	}
	if s.Component != "link" || s.ID != "link-xyz" || s.Counters["tunnels_established"] != 2 {
		t.Errorf("record %+v, want component link, id link-xyz and the counters", s)
	}
}

// A route-only kit never joins the overlay: warp enroll refuses its key before
// anything is allocated.
func TestEnrollRefusesARouteOnlyKey(t *testing.T) {
	h, _, key, _ := routeOnlyKit(t)
	rec := callAs(h.Enroll, http.MethodPost, "/api/warp/enroll", `{"public_key":"pk"}`, key)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "route-only") {
		t.Fatalf("status %d (%s), want 403 naming the route-only kit", rec.Code, rec.Body.String())
	}
}
