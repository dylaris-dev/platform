package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	"github.com/gorilla/mux"
)

// What an operator may do on a machine that belongs to somebody else.
//
// The owner's rule (roadmap/node-ownership-and-access.md): an admin may
// decommission a user's node, and may NOT see its servers or reach into its
// disk. Every read of a machine's contents used to short-circuit on admin, so
// these tests are written from the ADMIN's side - that is the caller the old
// code waved through, and a table that only tried a stranger would have passed
// against it.
//
// Each case also has a platform-node twin that must get PAST the new guard. A
// guard that refused everything would pass the owned-node case on its own.

const (
	visOwner = "11111111-1111-1111-1111-111111111111"
	visAdmin = "22222222-2222-2222-2222-222222222222"
)

func visOwnedNode() *models.Node {
	owner := visOwner
	return &models.Node{ID: 7, Status: "online", OwnerID: &owner}
}

func visPlatformNode() *models.Node {
	return &models.Node{ID: 7, Status: "online"}
}

// visState builds handler state around the shared fake, with BYON switched on
// so ownership is in force. server and serverErr are what GetServerByUUID answers.
func visState(node *models.Node, server *models.Server, serverErr error) *AppState {
	return &AppState{
		Store:        &orphanAssignFakeStore{node: node, server: server, err: serverErr},
		FeatureFlags: tenancyFeatureFlags(true),
	}
}

func visAdminRequest(method, target string, vars map[string]string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := context.WithValue(r.Context(), "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", visAdmin)
	return mux.SetURLVars(r.WithContext(ctx), vars)
}

// TestDiskToolsDoNotOpenACustomersMachineToAnAdmin covers the five disk routes.
// With GRPCRegistry nil, a request that gets past every guard stops at "gRPC
// not available" (503) - which is how the platform twin proves it was not
// refused by the node check.
func TestDiskToolsDoNotOpenACustomersMachineToAnAdmin(t *testing.T) {
	orphanVars := map[string]string{"nodeId": "7", "uuid": "orphan-1"}
	routes := []struct {
		name string
		call func(h *NodeHandler, w http.ResponseWriter)
		// passCode is what the PLATFORM node answers once past the guard.
		passCode int
	}{
		{"list orphan files", func(h *NodeHandler, w http.ResponseWriter) {
			h.ListOrphanFiles(w, visAdminRequest("GET", "/x", orphanVars))
		}, http.StatusServiceUnavailable},
		{"read an orphan file", func(h *NodeHandler, w http.ResponseWriter) {
			h.GetOrphanFileContent(w, visAdminRequest("GET", "/x?path=server.properties", orphanVars))
		}, http.StatusServiceUnavailable},
		{"inspect an orphan", func(h *NodeHandler, w http.ResponseWriter) {
			h.InspectOrphan(w, visAdminRequest("GET", "/x", orphanVars))
		}, http.StatusServiceUnavailable},
		{"delete an orphan folder", func(h *NodeHandler, w http.ResponseWriter) {
			h.DeleteOrphanedFolder(w, visAdminRequest("DELETE", "/x?uuid=orphan-1", map[string]string{"id": "7"}))
		}, http.StatusServiceUnavailable},
		{"disk analysis", func(h *NodeHandler, w http.ResponseWriter) {
			h.GetDiskAnalysis(w, visAdminRequest("GET", "/x", map[string]string{"id": "7"}))
		}, http.StatusOK},
	}

	for _, rt := range routes {
		t.Run(rt.name+"/a user's machine is not found", func(t *testing.T) {
			h := &NodeHandler{state: visState(visOwnedNode(), nil, sql.ErrNoRows)}
			rw := httptest.NewRecorder()
			rt.call(h, rw)
			if rw.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
			}
		})
		t.Run(rt.name+"/a platform machine is still the operator's", func(t *testing.T) {
			h := &NodeHandler{state: visState(visPlatformNode(), nil, sql.ErrNoRows)}
			rw := httptest.NewRecorder()
			rt.call(h, rw)
			if rw.Code != rt.passCode {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, rt.passCode, rw.Body.String())
			}
		})
	}
}

// TestOrphanRoutesRefuseALiveServer is the other half of the orphan hole, and
// it is node-independent: the uuid of a LIVE server read that server's files
// through nodes.read, past its own files.read. On a platform node, as the
// operator, so ownership cannot be what refuses it.
func TestOrphanRoutesRefuseALiveServer(t *testing.T) {
	vars := map[string]string{"nodeId": "7", "uuid": "live-1"}
	calls := map[string]func(h *NodeHandler, w http.ResponseWriter){
		"list": func(h *NodeHandler, w http.ResponseWriter) { h.ListOrphanFiles(w, visAdminRequest("GET", "/x", vars)) },
		"read": func(h *NodeHandler, w http.ResponseWriter) {
			h.GetOrphanFileContent(w, visAdminRequest("GET", "/x?path=a", vars))
		},
		"inspect": func(h *NodeHandler, w http.ResponseWriter) { h.InspectOrphan(w, visAdminRequest("GET", "/x", vars)) },
		"delete": func(h *NodeHandler, w http.ResponseWriter) {
			h.DeleteOrphanedFolder(w, visAdminRequest("DELETE", "/x?uuid=live-1", map[string]string{"id": "7"}))
		},
	}
	for name, call := range calls {
		t.Run(name+"/a server row means it is not an orphan", func(t *testing.T) {
			h := &NodeHandler{state: visState(visPlatformNode(), &models.Server{ID: 3, UUID: "live-1"}, nil)}
			rw := httptest.NewRecorder()
			call(h, rw)
			if rw.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rw.Code, rw.Body.String())
			}
		})
		// A store that cannot answer is not "no such server". The delete route
		// used to discard this error and delete the folder anyway.
		t.Run(name+"/a database fault is not an orphan", func(t *testing.T) {
			h := &NodeHandler{state: visState(visPlatformNode(), nil, errors.New("connection refused"))}
			rw := httptest.NewRecorder()
			call(h, rw)
			if rw.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (%s)", rw.Code, rw.Body.String())
			}
		})
	}
}

// TestAdoptingAnOrphanOnACustomersMachineIsRefused: adopting inspects the folder
// and writes a server row onto that hardware for an owner of the admin's choice.
func TestAdoptingAnOrphanOnACustomersMachineIsRefused(t *testing.T) {
	h := &NodeHandler{state: visState(visOwnedNode(), nil, sql.ErrNoRows)}
	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/disk/orphans/assign", bytes.NewReader(orphanAssignBody()))
	req = req.WithContext(visAdminRequest(http.MethodPost, "/x", nil).Context())
	h.AssignOrphan(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
	}
}

// TestForceDeleteNeverTouchesACustomersMachine: it deletes every server on the
// node and answers with their names. The platform twin is ONLINE so it stops at
// the existing "cannot force-delete an online node" check, which proves it got
// past the new one without deleting anything.
func TestForceDeleteNeverTouchesACustomersMachine(t *testing.T) {
	cases := []struct {
		name string
		node *models.Node
		byon bool
		want int
	}{
		{"a user's machine, BYON on", visOwnedNode(), true, http.StatusConflict},
		{"a platform machine", visPlatformNode(), true, http.StatusBadRequest},
		// With BYON off an owner_id means nothing (decision D6 is still open),
		// so this step deliberately leaves that case as it was.
		{"a user's machine, BYON off", visOwnedNode(), false, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := visState(c.node, nil, sql.ErrNoRows)
			st.FeatureFlags = tenancyFeatureFlags(c.byon)
			h := &NodeHandler{state: st}
			rw := httptest.NewRecorder()
			h.ForceDeleteNode(rw, visAdminRequest("DELETE", "/x", map[string]string{"id": "7"}))
			if rw.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}

// TestNodeCPUDoesNotLetResourceRightsReachACustomersMachine: every admin also
// carries CanChangeResources, so refusing the admin in canManageNode alone would
// have been undone by the second arm of the same condition.
func TestNodeCPUDoesNotLetResourceRightsReachACustomersMachine(t *testing.T) {
	admin := &models.User{ID: visAdmin, IsAdmin: true, Role: "admin"}
	cases := []struct {
		name string
		node *models.Node
		want int
	}{
		{"a user's machine", visOwnedNode(), http.StatusForbidden},
		// Past the gate, stopped by the nil pinning service.
		{"a platform machine", visPlatformNode(), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &AppState{
				Store:        &orphanAssignFakeStore{node: c.node, user: admin},
				FeatureFlags: tenancyFeatureFlags(true),
			}
			h := &CPUPinningHandler{state: st}
			rw := httptest.NewRecorder()
			h.GetNodeCPU(rw, visAdminRequest("GET", "/x", map[string]string{"id": "7"}))
			if rw.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", rw.Code, c.want, rw.Body.String())
			}
		})
	}
}
