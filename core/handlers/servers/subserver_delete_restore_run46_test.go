package servers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/gorilla/mux"
)

func (f *subServerDeleteFakeStore) ListBackupRestores(serverID, limit int) ([]models.BackupRestore, error) {
	return f.restores, f.restoresErr
}

// R46-03: a restore still waiting in the node's stream holds no node_busy, so
// a delete let through beside it was queued behind the restore on the node.
// The restore's success report wrote the sub-server's install record and mod
// rows back from the manifest, and the delete then removed the directory under
// them. Refused while the server has an open restore row.
func TestDeleteSubServerRefusesWhileARestoreIsOpen(t *testing.T) {
	for _, tt := range []struct {
		name       string
		restores   []models.BackupRestore
		err        error
		wantStatus int
		wantBody   string
	}{
		{name: "no restores", wantStatus: http.StatusOK},
		{name: "only finished restores", restores: []models.BackupRestore{{Status: "success"}, {Status: "failed"}}, wantStatus: http.StatusOK},
		{name: "a queued restore", restores: []models.BackupRestore{{Status: "queued"}, {Status: "success"}}, wantStatus: http.StatusConflict, wantBody: "restore_running"},
		{name: "a running restore", restores: []models.BackupRestore{{Status: "running"}}, wantStatus: http.StatusConflict, wantBody: "restore_running"},
		{name: "the lookup fails", err: errors.New("db down"), wantStatus: http.StatusServiceUnavailable, wantBody: "restore"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := &subServerDeleteFakeStore{admin: true, restores: tt.restores, restoresErr: tt.err}
			h := &ServerHandler{d: testDeps(Deps{Store: fs, Events: services.NewSystemEventsPublisher(nil)})}
			req := httptest.NewRequest(http.MethodDelete, "/api/servers/7/sub-servers/creative", nil)
			ctx := context.WithValue(req.Context(), "userID", "alice")
			ctx = context.WithValue(ctx, "isAdmin", true)
			req = mux.SetURLVars(req.WithContext(ctx), map[string]string{"id": "7", "subServerName": "creative"})
			rw := httptest.NewRecorder()
			h.DeleteSubServer(rw, req)
			if rw.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rw.Code, tt.wantStatus, rw.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(rw.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rw.Body.String(), tt.wantBody)
			}
			if tt.wantStatus != http.StatusOK && (fs.installCleared != "" || fs.modsCleared != "") {
				t.Error("the refused delete still dropped the install record or the mod rows")
			}
		})
	}
}
