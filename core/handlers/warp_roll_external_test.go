package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/store"
)

type rollExternalStore struct {
	store.Store
	key    *store.WarpAPIKey
	rolled string
}

func (f *rollExternalStore) GetWarpAPIKeyByID(int) (*store.WarpAPIKey, error) {
	if f.key == nil {
		return nil, sql.ErrNoRows
	}
	return f.key, nil
}
func (f *rollExternalStore) GetNodeByID(int) (*models.Node, error) {
	return &models.Node{ID: 9, Token: "tok"}, nil
}
func (f *rollExternalStore) RollWarpAPIKeyHash(nodeID, _ string) error {
	f.rolled = nodeID
	return nil
}

// Only an External node's own key rolls here, and only while its node says it
// moves the Link onto the new secret: one that does not would keep its Link on
// the old one, which stops working when the grace runs out.
func TestRollExternalNodeKey(t *testing.T) {
	for _, c := range []struct {
		name  string
		key   *store.WarpAPIKey
		hb    string // the bound node's heartbeat; "" = none
		admin bool
		want  int
	}{
		{"not an admin", &store.WarpAPIKey{ID: 1, NodeID: "node-a"}, "", false, http.StatusForbidden},
		{"unknown key", nil, "", true, http.StatusNotFound},
		{"a customer's key", &store.WarpAPIKey{ID: 1, NodeID: "node-a", OwnerID: "u1"}, "", true, http.StatusNotFound},
		{"a route-only key", &store.WarpAPIKey{ID: 1, NodeID: "link-a"}, "", true, http.StatusNotFound},
		{"not enrolled yet", &store.WarpAPIKey{ID: 1, NodeID: "node-a"}, "", true, http.StatusOK},
		{"node does not swap (too old, or updater off)", &store.WarpAPIKey{ID: 1, NodeID: "node-a", BoundNodeID: 9}, `{"releaseVersion":"2026.10.12"}`, true, http.StatusConflict},
		{"node offline", &store.WarpAPIKey{ID: 1, NodeID: "node-a", BoundNodeID: 9}, "", true, http.StatusConflict},
		{"node swaps", &store.WarpAPIKey{ID: 1, NodeID: "node-a", BoundNodeID: 9}, `{"releaseVersion":"2026.10.12","linkKeySwap":true}`, true, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			rdb, mr := newQuotaHTTPRedis(t)
			if c.hb != "" {
				mr.Set("dylaris:discovery:tok", c.hb)
			}
			st := &rollExternalStore{key: c.key}
			h := &WarpHandler{state: &AppState{Store: st, Redis: rdb}}
			req := httptest.NewRequest(http.MethodPost, "/api/admin/warp/keys/1/roll", nil)
			req = mux.SetURLVars(req, map[string]string{"id": "1"})
			req = req.WithContext(context.WithValue(req.Context(), "isAdmin", c.admin))
			rec := httptest.NewRecorder()
			h.RollExternalNodeKey(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body, c.want)
			}
			if (st.rolled != "") != (c.want == http.StatusOK) {
				t.Errorf("rolled %q with status %d", st.rolled, rec.Code)
			}
			if c.want == http.StatusOK {
				var out struct {
					WarpKey string `json:"warp_key"`
				}
				if json.Unmarshal(rec.Body.Bytes(), &out); len(out.WarpKey) < 32 {
					t.Errorf("no new key in %s", rec.Body)
				}
			}
		})
	}
}
