package packs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// packScopeFakeStore holds two packs owned by different users, each with one
// build, so a test can pair one user's packId with the other user's buildId.
type packScopeFakeStore struct {
	store.Store
	packs  map[int]*models.Pack
	builds map[int]*models.PackBuild
}

func (f *packScopeFakeStore) GetPack(id int) (*models.Pack, error) {
	return f.packs[id], nil
}

func (f *packScopeFakeStore) GetPackBuild(id int) (*models.PackBuild, error) {
	return f.builds[id], nil
}

func (f *packScopeFakeStore) ListBuildContent(buildID int) ([]models.BuildContentEntry, error) {
	e := models.BuildContentEntry{}
	e.Modversion.ID = buildID * 100
	e.Modversion.StorageKey = "modpacks/secret-key-of-build-" + strconv.Itoa(buildID)
	return []models.BuildContentEntry{e}, nil
}

func newPackScopeHandler() *PacksHandler {
	return &PacksHandler{state: &handlers.AppState{Store: &packScopeFakeStore{
		packs: map[int]*models.Pack{
			1: {ID: 1, OwnerID: "alice"},
			2: {ID: 2, OwnerID: "bob"},
		},
		builds: map[int]*models.PackBuild{
			10: {ID: 10, PackID: 1}, // alice's
			20: {ID: 20, PackID: 2}, // bob's
		},
	}}}
}

func packContentRequest(t *testing.T, userID, packID, buildID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/packs/"+packID+"/builds/"+buildID+"/content", nil)
	req = req.WithContext(context.WithValue(req.Context(), "userID", userID))
	return mux.SetURLVars(req, map[string]string{"id": packID, "buildId": buildID})
}

// TestListContent_RejectsAForeignBuildUnderAnOwnedPack is the finding: owning
// the pack named in the path was the only check, and it says nothing about the
// build. Alice owns pack 1, so she passed the ownsPack gate, and the handler
// then listed whatever buildId she asked for - including bob's build 20, whose
// entries carry the object-storage keys.
func TestListContent_RejectsAForeignBuildUnderAnOwnedPack(t *testing.T) {
	h := newPackScopeHandler()
	rw := httptest.NewRecorder()
	h.ListContent(rw, packContentRequest(t, "alice", "1", "20"))

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; alice must not read bob's build through her own pack (body: %s)",
			rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), "secret-key-of-build-20") {
		t.Error("response leaked the foreign build's content entries")
	}
}

// TestListContent_AllowsTheOwnPackAndBuild is the control: the guard must not
// break the ordinary case.
func TestListContent_AllowsTheOwnPackAndBuild(t *testing.T) {
	h := newPackScopeHandler()
	rw := httptest.NewRecorder()
	h.ListContent(rw, packContentRequest(t, "alice", "1", "10"))

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "secret-key-of-build-10") {
		t.Errorf("own build content missing from the response: %s", rw.Body.String())
	}
}

// TestListContent_StillRefusesAForeignPack is the second control: the
// pre-existing ownsPack gate must keep working.
func TestListContent_StillRefusesAForeignPack(t *testing.T) {
	h := newPackScopeHandler()
	rw := httptest.NewRecorder()
	h.ListContent(rw, packContentRequest(t, "alice", "2", "20"))

	if rw.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rw.Code, rw.Body.String())
	}
}

// TestListContent_AdminKeepsCrossOwnerReadWithinTheRightPack pins the
// behaviour the fix deliberately did NOT change: ownsPack grants admins read
// access to a pack they do not own, and the new binding only constrains WHICH
// build may be read under it.
func TestListContent_AdminKeepsCrossOwnerReadWithinTheRightPack(t *testing.T) {
	h := newPackScopeHandler()
	req := packContentRequest(t, "carol", "2", "20")
	req = req.WithContext(context.WithValue(req.Context(), "isAdmin", true))
	rw := httptest.NewRecorder()
	h.ListContent(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; admin read of another owner's pack is pre-existing behaviour (body: %s)",
			rw.Code, rw.Body.String())
	}

	// ...but a mismatched build is still refused, admin or not.
	rw2 := httptest.NewRecorder()
	req2 := packContentRequest(t, "carol", "2", "10")
	req2 = req2.WithContext(context.WithValue(req2.Context(), "isAdmin", true))
	h.ListContent(rw2, req2)
	if rw2.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a build outside the named pack", rw2.Code)
	}
}

// No collaborator invites in this fake.
func (*packScopeFakeStore) PackRightsFor(string, string, int) (store.PackRights, error) {
	return store.PackRights{}, nil
}
