package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// The defect this pins, measured on production: with the cutoff enforced only
// on Start, a suspended tenant called POST /servers/{id}/setup and the node
// installed and BOOTED the server. Stopped, suspended, one call, online again -
// the customer undoing their own cutoff.
//
// The handler is built with a nil Redis and a nil Queue on purpose. Setup
// claims an install cooldown in Redis before it does anything else, so a
// refusal that arrives without touching either proves the guard runs BEFORE
// any work is started, not somewhere after it.
func TestSetupServerRefusedWhileTheOwnerIsSuspended(t *testing.T) {
	fs := &serverPowerFakeStore{
		server:  &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerID: "u1", OwnerName: "alice", NodeID: 7},
		billing: &store.UserBilling{UserID: "u1", Status: "suspended"},
	}
	h := &ServerHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}

	body, _ := json.Marshal(map[string]any{
		"subServerName": "main",
		"installer":     map[string]string{"type": "paper", "version": "1.21.1", "mcVersion": "1.21.1"},
	})
	r := httptest.NewRequest("POST", "/api/servers/1/setup", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "username", "alice")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "u1")
	rec := httptest.NewRecorder()

	h.SetupServer(rec, r.WithContext(ctx))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrBody(t, rec); msg != suspendedMessage {
		t.Fatalf("message = %q, want the suspension message", msg)
	}
}

// An admin must still be able to install for a suspended tenant: support keeps
// control of the machines, which is the same exemption the power path makes.
// This one runs far enough to need Redis, so it gets the real miniredis.
func TestSetupServerStillWorksForAnAdminWhileTheOwnerIsSuspended(t *testing.T) {
	fs := &serverPowerFakeStore{
		server:  &models.Server{ID: 1, UUID: "srv-uuid", Status: "stopped", OwnerID: "u1", OwnerName: "alice", NodeID: 7},
		billing: &store.UserBilling{UserID: "u1", Status: "suspended"},
		node:    &models.Node{ID: 7, Token: "node-token-1", Name: "n1", Status: "online"},
	}
	h := newServerPowerHandler(fs, newServerPowerRedis(t))

	body, _ := json.Marshal(map[string]any{
		"subServerName": "main",
		"installer":     map[string]string{"type": "paper", "version": "1.21.1", "mcVersion": "1.21.1"},
	})
	r := httptest.NewRequest("POST", "/api/servers/1/setup", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"id": "1"})
	ctx := context.WithValue(r.Context(), "username", "root")
	ctx = context.WithValue(ctx, "isAdmin", true)
	ctx = context.WithValue(ctx, "userID", "admin1")
	rec := httptest.NewRecorder()

	h.SetupServer(rec, r.WithContext(ctx))

	if rec.Code == http.StatusForbidden && decodeErrBody(t, rec) == suspendedMessage {
		t.Fatalf("an admin was stopped by the tenant's suspension: %s", rec.Body.String())
	}
}
