package servers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

// R30-02: the delete checked only the name's characters, so any name was
// queued, and each one parked a node worker waiting out a backup. A name the
// server does not have is refused like the switch refuses it.
func TestDeleteSubServerRefusesANameTheServerDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name, sub   string
		installsErr error
		wantStatus  int
	}{
		{"a sub-server the install records know", "creative", nil, http.StatusOK},
		{"the active sub-server", "survival", nil, http.StatusOK},
		{"a name the server does not have", "ghost", nil, http.StatusBadRequest},
		{"nothing can answer", "ghost", errors.New("db down"), http.StatusServiceUnavailable},
		// Core's own record names the active one, so a store failure does not
		// stand in the way of deleting it.
		{"the active one while nothing can answer", "survival", errors.New("db down"), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &subServerDeleteFakeStore{admin: true, installsErr: tc.installsErr}
			h := &ServerHandler{d: testDeps(Deps{Store: fs, Redis: handlertest.RedisClient(t), Events: services.NewSystemEventsPublisher(nil)})}
			req := httptest.NewRequest(http.MethodDelete, "/api/servers/7/sub-servers/"+tc.sub, nil)
			ctx := context.WithValue(req.Context(), "userID", "alice")
			ctx = context.WithValue(ctx, "isAdmin", true)
			req = mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": "7", "subServerName": tc.sub})
			rw := httptest.NewRecorder()
			h.DeleteSubServer(rw, req)
			if rw.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rw.Code, tc.wantStatus, rw.Body.String())
			}
			if tc.wantStatus != http.StatusOK && (fs.statusSet != "" || fs.installCleared != "") {
				t.Error("the refused delete still reset the row or dropped the install record")
			}
		})
	}
}
