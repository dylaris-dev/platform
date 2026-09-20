package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

type createInputStore struct {
	store.Store
	user *models.User
}

func (f *createInputStore) GetUserByID(id string) (*models.User, error) {
	if f.user != nil && f.user.ID == id {
		return f.user, nil
	}
	return nil, errNoSuchUser
}

var errNoSuchUser = &storeMiss{}

type storeMiss struct{}

func (*storeMiss) Error() string { return "no such user" }

func createServerReq(body map[string]any, userID string, isAdmin bool) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/servers", bytes.NewReader(b))
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	ctx = context.WithValue(ctx, "username", "someone")
	return r.WithContext(ctx)
}

// A node is named by its numeric id here and by its uuid everywhere else in the
// API, and the mistake used to be answered with "Node not found" - a row that
// is missing, rather than the field that is wrong. Sscanf left the id at 0 and
// the lookup did the rest.
func TestCreateServerRejectsANodeUUIDByName(t *testing.T) {
	h := &ServerHandler{state: &AppState{Store: &createInputStore{user: &models.User{ID: "u1"}}}}
	rec := httptest.NewRecorder()
	h.CreateServer(rec, createServerReq(map[string]any{
		"uuid":   "3f1f0e8a-6c1e-4d8a-9c4a-0b1d2e3f4a5b",
		"name":   "s",
		"nodeId": "597de090-d6b5-4c66-b617-d13b676ecb53",
		"docker": map[string]any{"ram": 1024, "cpuLimit": 1.0, "diskLimit": 2147483648},
	}, "u1", true))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Message == "Node not found" || out.Message == "" {
		t.Fatalf("message = %q; it has to name the field, not the missing row", out.Message)
	}
}

// Leaving the owner out means "mine". An empty value used to travel on into the
// lookup and be answered with "Owner not found".
func TestEffectiveOwnerID(t *testing.T) {
	cases := []struct {
		name              string
		requested, caller string
		want              string
	}{
		{"named owner wins", "someone-else", "admin-1", "someone-else"},
		{"no owner means the caller", "", "admin-1", "admin-1"},
		{"blank owner means the caller", "   ", "admin-1", "admin-1"},
		{"no caller either, and the lookup below says so", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveOwnerID(c.requested, c.caller); got != c.want {
				t.Errorf("effectiveOwnerID(%q, %q) = %q, want %q", c.requested, c.caller, got, c.want)
			}
		})
	}
}
