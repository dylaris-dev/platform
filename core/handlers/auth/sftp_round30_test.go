package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/beamaccess"

	beamauth "dylaris-pkg/beam/auth"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

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
	h := NewPasswordResetHandler(Deps{Store: fs, Redis: rdb})
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
	beamaccess.StampForUser(context.Background(), &beamStampUserStore{resetRevokeStore: &resetRevokeStore{}, admin: true}, rdb, "u1")
	if !mr.Exists(beamauth.AccessEpochKey("someone-elses")) {
		t.Fatal("an administrator's reach was not stamped")
	}
}
