package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// The machines tab shows the node's own state and, separately, the state of the
// Link that carries its players. Tested at the HANDLER, not only on the helper
// underneath it: a value that never reaches the response is exactly the kind of
// defect that leaves every test green (see kitInput in the panel).

type nodeListStore struct {
	store.Store
	settings map[string]string
	nodes    []models.Node
}

func (f *nodeListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *nodeListStore) ListNodes() ([]models.Node, error)   { return f.nodes, nil }
func (f *nodeListStore) CountServersByNode(int) (int, error) { return 0, nil }

func listNodesFor(t *testing.T, rdb *redis.Client, nodes []models.Node) map[string]*bool {
	t.Helper()
	fs := &nodeListStore{
		settings: map[string]string{"routing_mode": "gateway", "feature_byon_enabled": "true"},
		nodes:    nodes,
	}
	state := &AppState{
		Store:         fs,
		FeatureFlags:  services.NewFeatureFlags(fs),
		Redis:         rdb,
		ClusterSecret: "cs",
	}
	h := NewNodeHandler(state)
	r := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	rec := httptest.NewRecorder()
	h.GetNodes(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Nodes []struct {
			Token      string `json:"token"`
			LinkOnline *bool  `json:"linkOnline"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]*bool{}
	for _, n := range body.Nodes {
		out[n.Token] = n.LinkOnline
	}
	return out
}

func TestGetNodes_CarriesTheStateOfTheLinkThatServesEachNode(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	// One node on its derived token, one served by a Link the Hub named: asking
	// about the derived token of the second would report it as dead forever.
	derived := services.DeriveLinkToken("node-derived", "cs")
	mr.Set("online_link:"+derived, "1")
	mr.Set("online_link:hub-named", "1")

	owner := "alice"
	got := listNodesFor(t, rdb, []models.Node{
		{ID: 1, Token: "node-derived", Status: "online", OwnerID: &owner},
		{ID: 2, Token: "node-hubnamed", Status: "online", LinkToken: "hub-named"},
		{ID: 3, Token: "node-dark", Status: "online", OwnerID: &owner},
	})

	for token, want := range map[string]bool{"node-derived": true, "node-hubnamed": true, "node-dark": false} {
		v := got[token]
		if v == nil || *v != want {
			t.Errorf("%s linkOnline = %v, want %v", token, v, want)
		}
	}
}

// An in-cluster machine whose Link enrolled itself at the Hub runs under a
// token the Hub generated, and nothing derives it. Until the learner has named
// that link, the honest answer is nothing at all: a red badge on a machine that
// is serving players is worse than no badge.
func TestGetNodes_SaysNothingAboutAPlatformNodeWithNoNamedLink(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	got := listNodesFor(t, rdb, []models.Node{{ID: 1, Token: "platform-node", Status: "online"}})

	if v := got["platform-node"]; v != nil {
		t.Fatalf("linkOnline = %v, want absent for a platform node with no named link", *v)
	}
}

// Not knowing is not the same as no. A machine reported as "link not connected"
// because our own Redis is down sends its owner to debug a machine that is fine.
func TestGetNodes_SaysNothingAboutTheLinkWhenItCannotAsk(t *testing.T) {
	got := listNodesFor(t, nil, []models.Node{{ID: 1, Token: "node-a", Status: "online"}})

	if v := got["node-a"]; v != nil {
		t.Fatalf("linkOnline = %v, want absent", *v)
	}
}
