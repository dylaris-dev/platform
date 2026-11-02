package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// eventScopeFakeStore answers the resolver. Only ownership varies between the
// cases; nothing else grants anything.
type eventScopeFakeStore struct {
	server *models.Server
	err    error
}

func (f *eventScopeFakeStore) GetServerByID(int) (*models.Server, error) { return f.server, f.err }
func (f *eventScopeFakeStore) GetServerByUUID(string) (*models.Server, error) {
	return f.server, f.err
}
func (f *eventScopeFakeStore) GetPanelRole(int) (*store.PanelRole, error)   { return nil, nil }
func (f *eventScopeFakeStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }
func (f *eventScopeFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *eventScopeFakeStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return nil, nil
}
func (f *eventScopeFakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, nil
}

func eventScopeRequest(userID string, isAdmin bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/system/events", nil)
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "username", "someone")
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return r.WithContext(ctx)
}

// The channel is one fleet-wide fan-out, so every frame reached every session.
// Most frames say nothing, but the ones naming a serverId were telling every
// account about servers it cannot see. This is a signal bus, not a data path,
// so it fails OPEN - only a resolution that actually denies drops a frame.
func TestSystemEventsScope(t *testing.T) {
	const owner = "owner-1"
	srv := &models.Server{ID: 7, UUID: "srv", OwnerID: owner}

	tests := []struct {
		name    string
		payload string
		userID  string
		isAdmin bool
		store   *eventScopeFakeStore
		want    bool
		why     string
	}{
		{
			name:    "a frame naming no server",
			payload: `{"type":"servers.changed"}`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "it carries nothing to leak, and the panel needs it to refresh its own list",
		},
		{
			name:    "a platform-wide feature flag",
			payload: `{"type":"features.changed","payload":{"feature":"modpacks","enabled":true}}`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "any authenticated account can read the same flags from /api/system/features",
		},
		{
			name:    "someone else's server",
			payload: `{"type":"server_tabs.changed","payload":{"serverId":7}}`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: srv},
			want:    false,
			why:     "measured live: a non-admin received this for a server belonging to another account",
		},
		{
			name:    "the owner's own server",
			payload: `{"type":"server_tabs.changed","payload":{"serverId":7}}`,
			userID:  owner,
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "dropping this would stop the owner's own panel from refreshing",
		},
		{
			name:    "an admin",
			payload: `{"type":"server_mods.changed","payload":{"serverId":7}}`,
			userID:  "root",
			isAdmin: true,
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "an admin sees the fleet everywhere else too",
		},
		{
			name:    "a server the resolver cannot load",
			payload: `{"type":"server_tabs.changed","payload":{"serverId":7}}`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: nil},
			want:    false,
			why:     "an unknown server resolves to no capabilities, which is a real deny",
		},
		{
			name:    "a malformed frame",
			payload: `not json`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "unclassifiable means forward: this bus must not silently stop refreshing a panel",
		},
		{
			name:    "a serverId that is not a number",
			payload: `{"type":"x","payload":{"serverId":"seven"}}`,
			userID:  "stranger",
			store:   &eventScopeFakeStore{server: srv},
			want:    true,
			why:     "same reason - a shape we do not understand is not a denial",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &SystemEventsHandler{d: Deps{Authz: authz.NewResolver(tt.store)}}
			if got := h.MayReceive(eventScopeRequest(tt.userID, tt.isAdmin), tt.payload); got != tt.want {
				t.Errorf("MayReceive = %v, want %v: %s", got, tt.want, tt.why)
			}
		})
	}
}

// With no resolver wired there is nothing to decide with, and this bus must
// keep working rather than go quiet.
func TestSystemEventsScopeWithoutAResolver(t *testing.T) {
	h := &SystemEventsHandler{d: Deps{}}
	if !h.MayReceive(eventScopeRequest("stranger", false), `{"type":"x","payload":{"serverId":7}}`) {
		t.Error("a missing resolver silenced the event stream")
	}
}
