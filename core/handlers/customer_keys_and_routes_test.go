package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// A customer's warp node key and link kit are the credentials their machine
// boots with. Rolling one hands back the new plaintext, so an admin who could
// roll it could start warp or link AS that machine; revoking drops it off the
// overlay. The owner keeps both, and a platform key stays the operator's.
func TestAnAdminCannotRollOrRevokeACustomersWarpKeys(t *testing.T) {
	cases := []struct {
		name string
		call func(h *WarpHandler, w http.ResponseWriter, id string)
		id   string
	}{
		{"revoke a node key", func(h *WarpHandler, w http.ResponseWriter, id string) {
			h.RevokeNodeWarpKey(w, revokeReq(t, "/x", "nodeID", id, "admin-1", true))
		}, "node-abc"},
		{"roll a node key", func(h *WarpHandler, w http.ResponseWriter, id string) {
			h.RollNodeWarpKey(w, revokeReq(t, "/x", "nodeID", id, "admin-1", true))
		}, "node-abc"},
		{"revoke a link kit", func(h *WarpHandler, w http.ResponseWriter, id string) {
			h.RevokeLinkKit(w, revokeReq(t, "/x", "linkID", id, "admin-1", true))
		}, "link-abc"},
		{"roll a link kit", func(h *WarpHandler, w http.ResponseWriter, id string) {
			h.RollLinkKit(w, revokeReq(t, "/x", "linkID", id, "admin-1", true))
		}, "link-abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fs := newRevokeTestHandler(t)
			fs.settings["feature_byon_enabled"] = "true"
			fs.keysByNodeID[c.id] = &store.WarpAPIKey{ID: 7, NodeID: c.id, OwnerID: "customer-1"}
			rec := httptest.NewRecorder()
			c.call(h, rec, c.id)
			if rec.Code != http.StatusNotFound || len(fs.revoked) != 0 {
				t.Fatalf("status = %d, revoked = %v; an admin reached a customer's key (%s)", rec.Code, fs.revoked, rec.Body.String())
			}
		})
	}

	// The platform twin: an admin-minted key has no owner and stays the operator's.
	t.Run("revoke a platform node key", func(t *testing.T) {
		h, fs := newRevokeTestHandler(t)
		fs.settings["feature_byon_enabled"] = "true"
		fs.keysByNodeID["node-ours"] = &store.WarpAPIKey{ID: 8, NodeID: "node-ours"}
		rec := httptest.NewRecorder()
		h.RevokeNodeWarpKey(rec, revokeReq(t, "/x", "nodeID", "node-ours", "admin-1", true))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; an admin lost a platform key (%s)", rec.Code, rec.Body.String())
		}
	})
}

// DELETE /api/servers/{id}/routes/{domain} checks network.write against {id},
// and used to skip "does this route belong to {id}" for an admin. So a platform
// server's id deleted any route by name, a customer's included. Deleting any
// route by name has its own fleet route (topology.write).
func TestAServerRouteDeleteOnlyDeletesThatServersRoute(t *testing.T) {
	rdb, mr := newQuotaHTTPRedis(t)
	// The route exists - it belongs to another server. A check that only asked
	// "is there such a route" would let this through.
	mr.SAdd("sys:index:routes", "customer.example.com")
	mr.Set("route:customer.example.com", `{"server_uuid":"someone-elses-server"}`)
	st := &foreignFakeStore{node: visPlatformNode(), byon: "true"}
	h := NewGatewayHandler(&AppState{Store: st, Redis: rdb})
	r := foreignAdminRequest("DELETE", map[string]string{"id": "3", "domain": "customer.example.com"}, "")
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the delete went ahead for a route that is not this server's: %v", p)
			}
		}()
		h.DeleteServerRoute(rec, mux.SetURLVars(r, map[string]string{"id": "3", "domain": "customer.example.com"}))
	}()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

// node_id is not unique, so an admin key minted with a customer's link- or
// node- id was a second, owner-less row for that id, and link-boot answered it
// with the customer's tunnel token and Redis login. The panel never sends one.
func TestAnAdminKeyCannotCarryACustomersKeyID(t *testing.T) {
	for _, c := range []struct {
		nodeID string
		want   int
	}{
		{"link-abc", http.StatusBadRequest},
		{"node-abc", http.StatusBadRequest},
		{"", http.StatusOK},
	} {
		t.Run("node_id="+c.nodeID, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newWarpTestHandler(t).MintAPIKey(rec, adminMintReq(map[string]interface{}{"name": "k", "node_id": c.nodeID}))
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// Whatever minted it, an owner-less link- key is not a route-only kit.
func TestLinkBoot_AnOwnerlessLinkKeyIsNotAKit(t *testing.T) {
	h, _, _ := newNodeLinkHandler(t)
	rec := linkBootAs(h, store.WarpAPIKey{ID: 9, NodeID: "link-abc"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// The owned twin gets past that refusal (and stops later, at the fake's ACL).
	rec = linkBootAs(h, store.WarpAPIKey{ID: 10, NodeID: "link-abc", OwnerID: "owner-1"})
	if strings.Contains(rec.Body.String(), "Not a route-only link key") {
		t.Fatalf("an owned kit was refused as not a kit: %s", rec.Body.String())
	}
}

// The two remaining ownership checks that sat behind byonActive: an unreadable
// flag skipped them, so a settings fault let an admin place a server on a
// customer's machine by nodeId, or force-delete it with every server on it.
func TestPlacementAndForceDeleteFailClosed(t *testing.T) {
	fault := errors.New("connection refused")

	t.Run("create on a customer's machine", func(t *testing.T) {
		st := &foreignFakeStore{node: visOwnedNode(), settingErr: fault}
		h := &ServerHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(st)}}
		body := `{"uuid":"0f8c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b","name":"x","nodeId":"7","docker":{"ram":1024,"cpuLimit":1,"diskLimit":10240}}`
		rec := httptest.NewRecorder()
		h.CreateServer(rec, foreignAdminRequest("POST", nil, body))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("force delete a customer's machine", func(t *testing.T) {
		st := &orphanAssignFakeStore{node: visOwnedNode()}
		flags := &foreignFakeStore{settingErr: fault}
		h := &NodeHandler{state: &AppState{Store: st, FeatureFlags: services.NewFeatureFlags(flags)}}
		rec := httptest.NewRecorder()
		h.ForceDeleteNode(rec, visAdminRequest("DELETE", "/x", map[string]string{"id": "7"}))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
		}
	})
}
