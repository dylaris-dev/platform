package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/services"
)

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
