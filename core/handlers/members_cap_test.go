package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// capFakeStore is the slice of persistence the resolver needs, plus the server
// lookup capPermissions makes.
type capFakeStore struct {
	store.Store

	server       *models.Server
	serverErr    error
	serverGrant  *store.ServerGrant
	accountGrant *store.ServerGrant
	role         *store.ServerRole
	node         *models.Node // nil: a platform node
}

func (f *capFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node != nil {
		return f.node, nil
	}
	return &models.Node{ID: id}, nil
}
func (f *capFakeStore) GetSetting(key string) (string, error) {
	if key == "feature_byon_enabled" {
		return "true", nil
	}
	return "", nil
}

func (f *capFakeStore) GetServerByID(int) (*models.Server, error) {
	return f.server, f.serverErr
}
func (f *capFakeStore) GetServerByUUID(string) (*models.Server, error) { return f.server, nil }
func (f *capFakeStore) GetPanelRole(int) (*store.PanelRole, error)     { return nil, nil }
func (f *capFakeStore) GetServerRole(int) (*store.ServerRole, error)   { return f.role, nil }
func (f *capFakeStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}
func (f *capFakeStore) GetServerGrant(int, string) (*store.ServerGrant, error) {
	return f.serverGrant, nil
}
func (f *capFakeStore) GetAccountGrant(string, string) (*store.ServerGrant, error) {
	return f.accountGrant, nil
}

const (
	capOwner  = "11111111-1111-4111-8111-111111111111"
	capFriend = "22222222-2222-4222-8222-222222222222"
	capServer = 7
)

func capRequest(userID string, isAdmin bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/servers/7/members", nil)
	ctx := context.WithValue(r.Context(), "userID", userID)
	ctx = context.WithValue(ctx, "isAdmin", isAdmin)
	return r.WithContext(ctx)
}

func capHandler(fs *capFakeStore) *MemberHandler {
	// The resolver and the handler read the same fake, exactly as they read the
	// same store in production.
	return &MemberHandler{state: &AppState{Store: fs, Authz: authz.NewResolver(fs)}}
}

// An ACCOUNT-WIDE grantee has no per-server invite row - their grant is the row
// with server_id IS NULL. The old cap read the per-server row, got
// sql.ErrNoRows, and returned the request UNCAPPED, so they could delegate the
// full set on any server in the owner's realm whatever their own grant allowed.
// CreateInvite runs the result through MapLegacyInviteCaps, so those keys become
// real capability grants.
func TestAnAccountWideGranteeCannotDelegateMoreThanTheyHold(t *testing.T) {
	fs := &capFakeStore{
		server: &models.Server{ID: capServer, OwnerID: capOwner},
		// Console only, account-wide. No per-server row at all.
		accountGrant: &store.ServerGrant{
			UserID:       capFriend,
			CapOverrides: store.CapOverrides{Grant: store.MapLegacyInviteCaps(models.TabPermissions{Console: true, Members: true})},
		},
	}
	h := capHandler(fs)

	got := h.capPermissions(capRequest(capFriend, false), capServer, map[string]bool{
		"console": true, "files": true, "power": true, "members": true, "inherit": true,
	})

	if !got["console"] {
		t.Error("console was capped away, but the caller holds it")
	}
	if got["files"] {
		t.Error("files was delegated by a caller who does not hold it")
	}
	if got["power"] {
		t.Error("power was delegated by a caller who does not hold it")
	}
	if !got["members"] {
		t.Error("members was capped away, but the caller holds it")
	}
	if got["inherit"] {
		t.Error("inherit was delegated by a non-owner")
	}
}

// A per-server grantee must keep working exactly as before.
func TestAPerServerGranteeIsStillCappedToTheirOwnGrant(t *testing.T) {
	sid := capServer
	fs := &capFakeStore{
		server: &models.Server{ID: capServer, OwnerID: capOwner},
		serverGrant: &store.ServerGrant{
			ServerID:     &sid,
			UserID:       capFriend,
			CapOverrides: store.CapOverrides{Grant: store.MapLegacyInviteCaps(models.TabPermissions{Files: true, Members: true})},
		},
	}
	h := capHandler(fs)

	got := h.capPermissions(capRequest(capFriend, false), capServer, map[string]bool{
		"files": true, "console": true,
	})
	if !got["files"] {
		t.Error("files was capped away from a caller who holds it")
	}
	if got["console"] {
		t.Error("console was delegated by a caller who does not hold it")
	}
}

// The owner and an admin stay uncapped.
func TestTheOwnerAndAnAdminAreNotCapped(t *testing.T) {
	fs := &capFakeStore{server: &models.Server{ID: capServer, OwnerID: capOwner}}
	h := capHandler(fs)
	want := map[string]bool{"console": true, "files": true, "members": true, "inherit": true}

	for _, c := range []struct {
		name    string
		userID  string
		isAdmin bool
	}{
		{"the owner", capOwner, false},
		{"an admin", capFriend, true},
	} {
		got := h.capPermissions(capRequest(c.userID, c.isAdmin), capServer, want)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
}

// An admin reaches members.write on a customer's machine only through that
// customer's invite, so the invite caps what they may hand on. The admin flag
// used to return the request uncapped here, before the resolver was asked.
func TestAnAdminOnACustomersMachineIsCappedToTheInvite(t *testing.T) {
	sid := capServer
	owner := capOwner
	fs := &capFakeStore{
		server: &models.Server{ID: capServer, OwnerID: capOwner, NodeID: 3},
		node:   &models.Node{ID: 3, OwnerID: &owner},
		serverGrant: &store.ServerGrant{
			ServerID:     &sid,
			UserID:       capFriend,
			CapOverrides: store.CapOverrides{Grant: store.MapLegacyInviteCaps(models.TabPermissions{Console: true, Members: true})},
		},
	}
	h := capHandler(fs)
	h.state.Authz.SetForeignNode(h.state.NodeOwnedByOther)

	got := h.capPermissions(capRequest(capFriend, true), capServer, map[string]bool{
		"console": true, "files": true,
	})
	if !got["console"] {
		t.Error("console was capped away, but the invite grants it")
	}
	if got["files"] {
		t.Error("an admin on a customer's machine delegated files, which the customer never granted them")
	}
}

// A cap that cannot be computed must deny, not pass the request through. Every
// error path used to return the requested permissions unchanged, so a database
// blip widened a delegation.
func TestAFailedLookupDeniesInsteadOfPassingThrough(t *testing.T) {
	fs := &capFakeStore{serverErr: errors.New("connection reset by peer")}
	h := capHandler(fs)

	got := h.capPermissions(capRequest(capFriend, false), capServer, map[string]bool{
		"console": true, "files": true,
	})
	for k, v := range got {
		if v {
			t.Errorf("%s survived a failed server lookup", k)
		}
	}
	if len(got) != 2 {
		t.Errorf("the keys themselves were dropped: %v", got)
	}
}
