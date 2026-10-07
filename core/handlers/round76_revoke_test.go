package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// revokeStore: backend 5 linked to proxy 9, both owned by "owner"; "member"
// holds a narrow direct grant on 5 and an inherit grant on 9.
type revokeStore struct {
	store.Store
	deleted    bool
	proxyGrant bool
	staleRealm bool
}

func (f *revokeStore) GetServerByID(id int) (*models.Server, error) {
	if id == 9 {
		return &models.Server{ID: 9, OwnerID: "owner", ServerType: "proxy"}, nil
	}
	p := 9
	return &models.Server{ID: 5, OwnerID: "owner", ProxyID: &p}, nil
}
func (f *revokeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if serverID == 9 && f.proxyGrant {
		realm := "owner"
		if f.staleRealm {
			realm = "previous-owner"
		}
		return &store.ServerGrant{UserID: userID, OwnerUserID: realm, Inherit: true}, nil
	}
	return &store.ServerGrant{UserID: userID, OwnerUserID: "owner"}, nil
}
func (f *revokeStore) GetUserByUsername(name string) (*models.User, error) {
	return &models.User{ID: "target", Username: name}, nil
}
func (f *revokeStore) GetNodeByID(id int) (*models.Node, error) { return &models.Node{ID: id}, nil }
func (f *revokeStore) DeleteInvite(int, string) error           { f.deleted = true; return nil }
func (f *revokeStore) DeleteServerGrant(*int, string, string) error {
	f.deleted = true
	return nil
}
func (f *revokeStore) GetSetting(string) (string, error) { return "", nil }
func (f *revokeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return nil, nil
}
func (f *revokeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *revokeStore) GetServerRole(int) (*store.ServerRole, error)     { return nil, nil }
func (f *revokeStore) GetServerAuditState(int) (bool, bool, int, error) { return false, false, 0, nil }

// A direct grant on a server linked to a proxy can be the owner narrowing what
// the member inherits from the proxy. Deleting it widens their access, so a
// member holding members.delete - on their own grant, or a co-manager's - may
// not; the owner may.
func TestRevokingANarrowGrantUnderAProxyIsTheOwnersCall(t *testing.T) {
	cases := []struct {
		name       string
		caller     string
		proxyGrant bool
		staleRealm bool
		wantDelete bool
	}{
		{"member, inherit grant on the proxy", "member", true, false, false},
		{"member, nothing inherited", "member", false, false, true},
		{"member, stale inherit grant the resolver ignores", "member", true, true, true},
		{"owner", "owner", true, false, true},
	}
	for _, c := range cases {
		t.Run("RemoveMember/"+c.name, func(t *testing.T) {
			fs := &revokeStore{proxyGrant: c.proxyGrant, staleRealm: c.staleRealm}
			h := &MemberHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
			r := httptest.NewRequest(http.MethodDelete, "/api/servers/5/members/target", nil)
			r = mux.SetURLVars(r, map[string]string{"id": "5", "userId": "11111111-1111-1111-1111-111111111111"})
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.caller), "isAdmin", false))
			h.RemoveMember(httptest.NewRecorder(), r)
			if fs.deleted != c.wantDelete {
				t.Errorf("deleted = %v, want %v", fs.deleted, c.wantDelete)
			}
		})
		t.Run("RevokeGrant/"+c.name, func(t *testing.T) {
			fs := &revokeStore{proxyGrant: c.proxyGrant, staleRealm: c.staleRealm}
			h := &ServerRolesHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
			r := httptest.NewRequest(http.MethodDelete, "/api/grants", bytes.NewBufferString(`{"username":"t","serverId":5}`))
			r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", c.caller), "isAdmin", false))
			// The member needs members.delete on 5 to get as far as the check.
			if c.caller == "member" {
				fs2 := &memberDeleteStore{revokeStore: fs}
				h.state = &AppState{Store: fs2, Authz: authz.NewResolver(fs2)}
			}
			h.RevokeGrant(httptest.NewRecorder(), r)
			if fs.deleted != c.wantDelete {
				t.Errorf("deleted = %v, want %v", fs.deleted, c.wantDelete)
			}
		})
	}
}

// memberDeleteStore gives the caller members.delete on server 5 through a role.
type memberDeleteStore struct{ *revokeStore }

func (f *memberDeleteStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	g, err := f.revokeStore.GetServerGrant(serverID, userID)
	if g != nil && serverID == 5 {
		one := 1
		g.ServerRoleID = &one
	}
	return g, err
}
func (f *memberDeleteStore) GetServerRole(int) (*store.ServerRole, error) {
	return &store.ServerRole{ID: 1, Capabilities: []string{"members.delete"}}, nil
}

type demoListStore struct {
	store.Store
	settings map[string]string
}

func (f *demoListStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *demoListStore) SetSetting(k, v string) error        { f.settings[k] = v; return nil }

// The demo list holds UUIDs and a UUID can come back (CreateServer accepts one,
// adopting a leftover folder reuses it), so a deleted server left on the list
// made the next server with that UUID readable by every signed-in account.
func TestDeletedServersLeaveTheDemoList(t *testing.T) {
	fs := &demoListStore{settings: map[string]string{demoServerUUIDsSetting: `["keep","gone"]`}}
	s := &AppState{Store: fs, StoreEnabled: true}
	dropDemoServers(s, []string{"gone"})
	if isDemoServer(s, "gone") || !isDemoServer(s, "keep") {
		t.Fatalf("demo list after delete: %s", fs.settings[demoServerUUIDsSetting])
	}
}
