package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
)

type overviewStore struct {
	nodeListStore
	routes                  []store.CoreLinkRoute
	users                   int
	customerPeers, ourPeers int
}

func (f *overviewStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) { return f.routes, nil }
func (f *overviewStore) CountUsers() (int, error)                           { return f.users, nil }
func (f *overviewStore) CountWarpPeersByOwnership() (int, int, error) {
	return f.customerPeers, f.ourPeers, nil
}

type overviewBody struct {
	Nodes []struct {
		Token      string `json:"token"`
		LinkOnline *bool  `json:"linkOnline"`
	} `json:"nodes"`
	Customers struct {
		Warps struct {
			Total  int  `json:"total"`
			Online *int `json:"online"`
		} `json:"warps"`
	} `json:"customers"`
	Fleet fleetSummary `json:"fleet"`
}

func getOverview(t *testing.T, fs *overviewStore, rdb *redis.Client) overviewBody {
	t.Helper()
	h := NewInfrastructureHandler(&AppState{Store: fs, Redis: rdb, ClusterSecret: "cs"})
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/infrastructure/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body overviewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// The overview listed nodes without the state of the Link that serves them;
// only /api/nodes asked. Same helper on both now.
func TestOverviewCarriesLinkOnlineAndTheFleetCensus(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	mr.Set("online_link:hub-named", "1")
	// A warp leader's aggregate: 3 tunnels up.
	mr.Set("dylaris:gwbw:component:warp:leader-1", `{"gauges":{"peers_active":3}}`)

	alice, bob := "alice", "bob"
	fs := &overviewStore{
		nodeListStore: nodeListStore{nodes: []models.Node{
			{ID: 1, Token: "dc-1", Status: "online", LinkToken: "hub-named"},
			{ID: 2, Token: "dc-2", Status: "offline"},
			{ID: 3, Token: "ext-1", Status: "online", Tags: "external"},
			{ID: 4, Token: "byon-a1", Status: "online", OwnerID: &alice},
			{ID: 5, Token: "byon-a2", Status: "offline", OwnerID: &alice},
			{ID: 6, Token: "byon-b", Status: "online", OwnerID: &bob},
		}},
		routes: []store.CoreLinkRoute{
			{Domain: "a.example", OwnerID: "carol"}, {Domain: "b.example", OwnerID: "carol"},
			{Domain: "c.example", OwnerID: "bob"},
		},
		users:         42,
		customerPeers: 2, ourPeers: 1,
	}
	body := getOverview(t, fs, rdb)

	got := map[string]*bool{}
	for _, n := range body.Nodes {
		got[n.Token] = n.LinkOnline
	}
	if v := got["dc-1"]; v == nil || !*v {
		t.Errorf("dc-1 linkOnline = %v, want true", v)
	}
	if v := got["byon-b"]; v == nil || *v {
		t.Errorf("byon-b linkOnline = %v, want false (its derived link is not online)", v)
	}

	f := body.Fleet
	if f.Nodes.Platform != (kindCounts{2, 1}) || f.Nodes.External != (kindCounts{1, 1}) || f.Nodes.BYON != (kindCounts{3, 2}) {
		t.Errorf("nodes = %+v", f.Nodes)
	}
	if f.Customers.BYON != 2 || f.Customers.RouteOnly != 2 || f.Customers.Total != 3 || f.RouteOnlyRoutes != 3 {
		t.Errorf("customers = %+v, routes %d; want byon 2, routeOnly 2, total 3 (bob is both), routes 3", f.Customers, f.RouteOnlyRoutes)
	}
	if f.Users == nil || *f.Users != 42 {
		t.Errorf("users = %v, want 42", f.Users)
	}

	// Our own External peer is not a customer's, and the leaders' aggregate
	// cannot be split by owner, so no online figure while it is in there.
	if body.Customers.Warps.Total != 2 || body.Customers.Warps.Online != nil {
		t.Errorf("warps = %+v, want total 2 and no online", body.Customers.Warps)
	}
	fs.ourPeers = 0
	if w := getOverview(t, fs, rdb).Customers.Warps; w.Online == nil {
		t.Errorf("with only customer peers the aggregate is theirs; online = nil")
	}
}
