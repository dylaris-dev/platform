package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// backupStashStore is the smallest thing the resolver needs to grant the owner
// short-circuit on server 7.
type backupStashStore struct {
	store.Store
}

func (f *backupStashStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, OwnerID: "owner-1"}, nil
}
func (f *backupStashStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

// The stranger path walks the grant lookups; neither exists here, which is what
// makes them a stranger.
func (f *backupStashStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	return nil, nil
}
func (f *backupStashStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	return nil, nil
}

// The /backup-jobs and /backup-runs families name no server in their path, so
// RequireCap cannot gate them and never recorded them either. Measured on
// production: a delegate triggered a backup, RESTORED the server from it and
// deleted the run, and the owner's audit trail showed none of the three.
//
// hasServerAccess is where both the decision and the server id exist, so it is
// what hands them to the audit wrapper on the route.
func TestBackupAccessCheckFeedsTheAuditTrail(t *testing.T) {
	st := &backupStashStore{}
	h := &BackupHandler{state: &AppState{Store: st, Authz: authz.NewResolver(st)}}
	res := authz.NewResolver(st)

	var recorded []string
	res.SetServerWriteAudit(func(r *http.Request, serverID int, capID string, status int) {
		recorded = append(recorded, capID)
	})

	run := func(allow bool, capID string) {
		wrapped := res.AuditResolvedWrite(func(w http.ResponseWriter, req *http.Request) {
			if !h.hasServerAccess(req, 7, capID) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodPost, "/api/backup-runs/1/restore", nil)
		owner := "owner-1"
		if !allow {
			owner = "stranger"
		}
		req = req.WithContext(context.WithValue(req.Context(), "userID", owner))
		wrapped(httptest.NewRecorder(), req)
	}

	run(true, "backups.restore")
	if len(recorded) != 1 || recorded[0] != "backups.restore" {
		t.Fatalf("recorded %v, want one backups.restore row - restoring overwrites the world", recorded)
	}

	recorded = nil
	run(false, "backups.restore")
	if len(recorded) != 0 {
		t.Fatalf("recorded %v for a refused restore; a row for something that did not happen is worse than none", recorded)
	}
}
