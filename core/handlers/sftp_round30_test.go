package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"

	beamauth "dylaris-pkg/beam/auth"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type suspendedUploadStore struct{ quotaHTTPFakeStore }

func (suspendedUploadStore) GetUserBilling(string) (*store.UserBilling, error) {
	return &store.UserBilling{Status: "suspended"}, nil
}

// Beam refused a suspended tenant a ticket; the panel's upload took their file.
// An operator's key carries no override (round 26), so it is refused too.
func TestAnUploadToASuspendedTenantsServerIsRefused(t *testing.T) {
	rdb, _ := newQuotaHTTPRedis(t)
	st := suspendedUploadStore{quotaHTTPFakeStore{newCoreStorageHTTPFakeStore()}}
	h := &FileHandler{state: &AppState{Redis: rdb, Store: st, Authz: authz.NewResolver(st)}}
	req := newUploadRequest(t, "s1", 10)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyCtxKey{}, &apiKeyCtx{key: &models.APIKey{ID: 1}}))
	rw := httptest.NewRecorder()
	h.UploadFileHandler(rw, req)
	if rw.Code != http.StatusForbidden || !strings.Contains(rw.Body.String(), "suspended") {
		t.Fatalf("status %d (%s), want the suspension refusal", rw.Code, rw.Body.String())
	}
}

type beamStampUserStore struct {
	*resetRevokeStore
	admin bool
}

func (f *beamStampUserStore) GetUserByID(string) (*models.User, error) {
	return &models.User{ID: "u1", IsAdmin: f.admin}, nil
}
func (f *beamStampUserStore) ListServersForUser(_ string, isAdmin bool) ([]models.Server, error) {
	if isAdmin {
		return []models.Server{{UUID: "own"}, {UUID: "someone-elses"}}, nil
	}
	return []models.Server{{UUID: "own"}}, nil
}

// A password reset by mail stamped no server, so a Beam ticket minted by
// whoever had the account kept opening sessions until it expired.
func TestAPasswordResetEndsTheAccountsBeamTickets(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	fs := &beamStampUserStore{resetRevokeStore: &resetRevokeStore{}}
	h := NewPasswordResetHandler(&AppState{Store: fs, Redis: rdb})
	rec := httptest.NewRecorder()
	h.ResetPassword(rec, httptest.NewRequest("POST", "/api/auth/reset-password",
		strings.NewReader(`{"token":"abcdefghijklmnopqrstuvwxyz","password":"a-long-new-password"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if !mr.Exists(beamauth.AccessEpochKey("own")) {
		t.Fatal("the account's server was not stamped")
	}
}

// An administrator reaches every server; a demotion has to stamp all of them.
func TestStampingAnAdminCoversEveryServer(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	stampBeamAccessForUser(context.Background(), &AppState{Store: &beamStampUserStore{resetRevokeStore: &resetRevokeStore{}, admin: true}, Redis: rdb}, "u1")
	if !mr.Exists(beamauth.AccessEpochKey("someone-elses")) {
		t.Fatal("an administrator's reach was not stamped")
	}
}
