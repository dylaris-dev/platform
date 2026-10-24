package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"
)

type regionNeutralListStore struct {
	store.Store
	servers []models.Server
}

func (f *regionNeutralListStore) ListServersForUser(string, bool) ([]models.Server, error) {
	return f.servers, nil
}
func (f *regionNeutralListStore) GetSetting(string) (string, error) { return "", nil }

// The server list no longer filters by a per-user region set. A staff member
// with no regions (all_regions_access false, no user_regions rows) used to
// get an empty list for every server they did not own, even though the
// store's own access query had already returned them.
func TestTheServerListIsNotFilteredByRegion(t *testing.T) {
	h := &ServerHandler{state: &handlers.AppState{Store: &regionNeutralListStore{servers: []models.Server{
		{ID: 1, UUID: "eu-srv", Region: "eu", OwnerID: "someone-else"},
		{ID: 2, UUID: "us-srv", Region: "us", OwnerID: "someone-else"},
	}}}}
	r := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	ctx := context.WithValue(r.Context(), "userID", "staff-1")
	ctx = context.WithValue(ctx, "isAdmin", false)
	rec := httptest.NewRecorder()
	h.GetServers(rec, r.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Servers []models.Server `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Servers) != 2 {
		t.Fatalf("got %d servers, want both regardless of region: %+v", len(body.Servers), body.Servers)
	}
}
