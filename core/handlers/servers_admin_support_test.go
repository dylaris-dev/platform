package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// adminServersFakeStore answers only what GetAdminServers asks for. Anything
// else panics on the nil embedded interface, which is the point: a new call
// added to this handler has to be looked at.
type adminServersFakeStore struct {
	store.Store
	servers []models.Server
}

func (f *adminServersFakeStore) ListServersForUser(string, bool) ([]models.Server, error) {
	return f.servers, nil
}
func (f *adminServersFakeStore) CountInvitesPerServer() (map[int]int, error) { return nil, nil }

func adminServersRows(t *testing.T, isAdmin bool) []map[string]any {
	t.Helper()
	h := &ServerHandler{state: &AppState{Store: &adminServersFakeStore{servers: []models.Server{
		{ID: 1, Name: "srv", NodeName: "node-a", NodeAddress: "203.0.113.7"},
	}}}}
	r := httptest.NewRequest(http.MethodGet, "/api/admin/servers", nil)
	ctx := context.WithValue(r.Context(), "userID", "reader-1")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	rec := httptest.NewRecorder()
	h.GetAdminServers(rec, r.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	return body.Servers
}

// servers.read is held by the seeded 'support' role, which exists for read-only
// oversight and deliberately carries none of the high-privilege panel caps. The
// row embeds the whole server model, so it carried the NODE's address - our
// infrastructure, and nothing support needs to answer a ticket.
//
// The rest of the row still has to arrive, or this would be a different bug.
func TestTheAdminServerListGivesTheNodeAddressToAdminsOnly(t *testing.T) {
	t.Run("a non-admin reader", func(t *testing.T) {
		rows := adminServersRows(t, false)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0]["nodeAddress"] != "" {
			t.Errorf("nodeAddress = %v, want empty for a reader who is not an admin", rows[0]["nodeAddress"])
		}
		if rows[0]["name"] != "srv" || rows[0]["node"] != "node-a" {
			t.Errorf("the rest of the row did not arrive: %v", rows[0])
		}
	})

	t.Run("an admin", func(t *testing.T) {
		rows := adminServersRows(t, true)
		if rows[0]["nodeAddress"] != "203.0.113.7" {
			t.Errorf("nodeAddress = %v, want it kept for an admin", rows[0]["nodeAddress"])
		}
	})
}
