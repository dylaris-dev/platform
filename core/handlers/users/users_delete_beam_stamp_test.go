package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"

	beamauth "dylaris-pkg/beam/auth"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// stampWatchStore wipes the beam access stamp when the row is deleted, so a
// stamp present afterwards can only have been written after the commit.
type stampWatchStore struct {
	deleteRoutesFakeStore
	rdb *redis.Client
}

func (f *stampWatchStore) ListServersForUser(string, bool) ([]models.Server, error) {
	return []models.Server{{ID: 1, UUID: "srv-uuid"}}, nil
}
func (f *stampWatchStore) DeleteUser(id string) error {
	f.rdb.Del(context.Background(), beamauth.AccessEpochKey("srv-uuid"))
	return f.deleteRoutesFakeStore.DeleteUser(id)
}

// The deletion stamped the account's servers BEFORE the teardown and the row
// delete. A beam ticket for that account minted in between - its identity read
// after the stamp, the row still there - was newer than the stamp, and no later
// one came, so it kept opening sessions after the account was gone.
func TestDeleteUserStampsBeamAccessAfterTheRowIsGone(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &stampWatchStore{rdb: rdb}
	h := &UserHandler{d: testDeps(Deps{Store: fs, Redis: rdb})}

	rr := httptest.NewRecorder()
	h.DeleteUser(rr, deleteUserRequest())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if !mr.Exists(beamauth.AccessEpochKey("srv-uuid")) {
		t.Fatal("no beam access stamp after the account was deleted")
	}
}
