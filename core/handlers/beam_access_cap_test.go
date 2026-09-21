package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// A beam ticket is a full file-transfer credential: the node validates it and
// then serves list/read/WRITE/delete/rename over the whole server directory
// (node/beam_server.go), with no capability in BeamClaims to narrow it down.
// These tests pin that Core resolves beamAccessCap before minting one, and
// that the beam server list shows only what a ticket could actually open.
//
// The old gate accepted any server_invites row, so the presets below are the
// point: "viewer" is documented as READ-ONLY access to files and holds
// files.read without files.write; "operator" holds no file capability at all.
// Both had unrestricted write access through beam.

// GetNodeSecretEnc is what keys the beam LAN certificate: the ticket handler
// derives the pinned fingerprint from the node's PER-NODE secret rather than the
// fleet JWT secret. The embedded store.Store is nil, so without this the call
// panics instead of reporting "no secret". Empty = this node has none, which is
// the path that leaves the ticket with no direct hints.
func (f *beamAccessFakeStore) GetNodeSecretEnc(int) (string, error) { return "", nil }

type beamAccessFakeStore struct {
	store.Store

	server  *models.Server
	servers []models.Server
	node    *models.Node
	user    *models.User

	// grant + role back the resolver's delegation path: the grant's role
	// capabilities are what a delegated member actually holds. accountGrant is
	// the owner-realm variant (server_invites.server_id IS NULL).
	grant        *store.ServerGrant
	accountGrant *store.ServerGrant
	role         *store.ServerRole

	// inviteCalls counts GetInvite hits, so a perturbation that restores the
	// old membership check is visible as more than a status change.
	inviteCalls int

	// billingStatus of the server's OWNER, as the suspension guard reads it.
	billingStatus string
}

// billingStatus is what the suspension guard reads; empty means a paying
// account, so these cases test access and nothing else.
func (f *beamAccessFakeStore) GetUserBilling(userID string) (*store.UserBilling, error) {
	return &store.UserBilling{UserID: userID, Status: f.billingStatus}, nil
}

func (f *beamAccessFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if f.server != nil && f.server.UUID == uuid {
		return f.server, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetServerByID(id int) (*models.Server, error) {
	if f.server != nil && f.server.ID == id {
		return f.server, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if f.node != nil && f.node.ID == id {
		return f.node, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetUserByUsername(name string) (*models.User, error) {
	if f.user != nil && f.user.Username == name {
		return f.user, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) ListServersForUser(userID string, isAdmin bool) ([]models.Server, error) {
	return f.servers, nil
}

func (f *beamAccessFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	f.inviteCalls++
	return &models.ServerInvite{ID: 1, ServerID: serverID, UserID: userID}, nil
}

func (f *beamAccessFakeStore) GetSetting(key string) (string, error) { return "", nil }

// authz.Store surface beyond GetServerByID.
func (f *beamAccessFakeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetServerRole(id int) (*store.ServerRole, error) {
	if f.role != nil && f.role.ID == id {
		return f.role, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *beamAccessFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if f.grant != nil && f.grant.UserID == userID {
		return f.grant, nil
	}
	return nil, errors.New("not found")
}

func (f *beamAccessFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	if f.accountGrant != nil && f.accountGrant.UserID == userID {
		return f.accountGrant, nil
	}
	return nil, errors.New("not found")
}

// newBeamAccessHandler builds a BeamHandler whose min-version cache is
// pre-warmed. effectiveMinVersion otherwise fetches the signed release
// manifest from GitHub on every ticket request, which a unit test must not do;
// an unexpired cache entry short-circuits that fetch (beam_manifest.go).
func newBeamAccessHandler(fs *beamAccessFakeStore) *BeamHandler {
	h := NewBeamHandler(&AppState{Store: fs, Authz: authz.NewResolver(fs)}, "beam-test-secret", "test-cluster-secret")
	h.minCacheVal = ""
	h.minCacheAt = time.Now()
	return h
}

func newBeamAccessStore(capabilities []string, memberID string) *beamAccessFakeStore {
	roleID := 5
	fs := &beamAccessFakeStore{
		server: &models.Server{ID: 1, UUID: "srv-uuid", OwnerID: "owner-id", NodeID: 7},
		node:   &models.Node{ID: 7, Token: "node-token-1", Name: "n1"},
		user:   &models.User{ID: memberID, Username: "member"},
	}
	fs.servers = []models.Server{*fs.server}
	if capabilities != nil {
		fs.grant = &store.ServerGrant{UserID: memberID, ServerRoleID: &roleID}
		fs.role = &store.ServerRole{ID: roleID, Capabilities: capabilities}
	}
	return fs
}

func TestGetBeamTicket_RequiresBeamAccessCap(t *testing.T) {
	// Capability sets taken verbatim from authz/presets.go.
	viewer := []string{"overview.read", "console.read", "stats.read",
		"files.read", "config.read", "players.read", "backups.read"}
	operator := []string{"overview.read", "stats.read", "console.read", "console.send",
		"power.start", "power.stop", "power.restart", "rcon.exec", "players.read", "players.manage"}
	builder := append(append([]string{}, operator...),
		"files.read", "files.write", "config.read", "config.write", "mods.read", "mods.write", "sftp.access")

	cases := []struct {
		name       string
		caps       []string
		username   string
		userID     string
		isAdmin    bool
		wantTicket bool
	}{
		{"owner gets a ticket", nil, "owner", "owner-id", false, true},
		{"admin gets a ticket", nil, "root", "admin-id", true, true},
		{"builder holds sftp.access", builder, "member", "member-id", false, true},
		{"viewer is read-only on files and must not beam", viewer, "member", "member-id", false, false},
		{"operator holds no file capability at all", operator, "member", "member-id", false, false},
		{"member with no grant at all", nil, "member", "member-id", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(c.caps, c.userID)
			h := newBeamAccessHandler(fs)

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", c.username)
			ctx = context.WithValue(ctx, "isAdmin", c.isAdmin)
			ctx = context.WithValue(ctx, "userID", c.userID)
			rec := httptest.NewRecorder()

			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Success bool   `json:"success"`
				Ticket  string `json:"ticket"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)

			if c.wantTicket {
				if rec.Code != 200 || body.Ticket == "" {
					t.Fatalf("status=%d ticket=%q, want a signed ticket (body=%s)",
						rec.Code, body.Ticket, rec.Body.String())
				}
				return
			}
			if rec.Code != 403 {
				t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if body.Ticket != "" {
				t.Fatal("a refused caller was still handed a ticket")
			}
			// The old gate answered on membership alone. A fake that always
			// returns an invite proves the new gate is not consulting it.
			if fs.inviteCalls != 0 {
				t.Fatalf("GetInvite was consulted %d time(s); membership must not decide beam access",
					fs.inviteCalls)
			}
		})
	}
}

// An ACCOUNT-wide grant (server_invites.server_id IS NULL) is honoured by the
// resolver on every server of that owner, but GetInvite matches
// si.server_id = $1 and never returns one - so the old membership check
// refused exactly the delegation the access model says is valid.
func TestGetBeamTicket_HonoursAnAccountWideGrant(t *testing.T) {
	roleID := 9
	fs := newBeamAccessStore(nil, "member-id")
	fs.role = &store.ServerRole{ID: roleID, Capabilities: []string{"files.read", "files.write", "sftp.access"}}
	fs.accountGrant = &store.ServerGrant{UserID: "member-id", ServerRoleID: &roleID}
	h := newBeamAccessHandler(fs)

	r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
	ctx := context.WithValue(r.Context(), "username", "member")
	ctx = context.WithValue(ctx, "isAdmin", false)
	ctx = context.WithValue(ctx, "userID", "member-id")
	rec := httptest.NewRecorder()

	h.GetBeamTicket(rec, r.WithContext(ctx))

	var body struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || body.Ticket == "" {
		t.Fatalf("status=%d ticket=%q, want a ticket for an account-wide grant holding sftp.access (body=%s)",
			rec.Code, body.Ticket, rec.Body.String())
	}
}

// The server list feeds the beam client's picker, so it must not offer a
// server whose ticket request would be refused - nor hand out that server's
// node discovery token on the way.
func TestGetBeamServers_ListsOnlyWhatBeamCanOpen(t *testing.T) {
	cases := []struct {
		name     string
		caps     []string
		wantRows int
	}{
		{"member holding sftp.access sees the server", []string{"files.read", "sftp.access"}, 1},
		{"viewer-style member sees nothing", []string{"files.read", "overview.read"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(c.caps, "member-id")
			h := newBeamAccessHandler(fs)

			r := httptest.NewRequest("GET", "/api/beam/servers", nil)
			ctx := context.WithValue(r.Context(), "username", "member")
			ctx = context.WithValue(ctx, "isAdmin", false)
			ctx = context.WithValue(ctx, "userID", "member-id")
			rec := httptest.NewRecorder()

			h.GetBeamServers(rec, r.WithContext(ctx))

			var body struct {
				Servers []BeamServerInfo `json:"servers"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
			}
			if len(body.Servers) != c.wantRows {
				t.Fatalf("servers = %d, want %d (body=%s)", len(body.Servers), c.wantRows, rec.Body.String())
			}
			for _, s := range body.Servers {
				if s.NodeID == "" {
					t.Fatal("a listed server carried no node discovery id")
				}
			}
		})
	}
}

// A beam ticket is a working channel into the node, so a tenant who is cut off
// for non-payment must not get one. Measured on production before this guard
// existed: a suspended account minted a ticket and the relay spliced it through
// to the node, which is the cutoff being undone by the person it applies to.
//
// The owner case is the one that matters: the guard reads the SERVER's owner,
// not the caller, so an admin helping out is unaffected and a delegated member
// of a suspended owner is stopped just the same.
func TestGetBeamTicket_RefusedWhileTheOwnerIsSuspended(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		isAdmin    bool
		wantTicket bool
	}{
		{"a paying owner gets one", "active", false, true},
		{"grace period is not a cutoff", "past_due", false, true},
		{"a suspended owner does not", "suspended", false, false},
		{"an admin still does", "suspended", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newBeamAccessStore(nil, "owner-id")
			fs.billingStatus = c.status
			h := newBeamAccessHandler(fs)

			r := httptest.NewRequest("GET", "/api/beam/ticket?server_uuid=srv-uuid", nil)
			ctx := context.WithValue(r.Context(), "username", "owner")
			ctx = context.WithValue(ctx, "isAdmin", c.isAdmin)
			ctx = context.WithValue(ctx, "userID", "owner-id")
			rec := httptest.NewRecorder()

			h.GetBeamTicket(rec, r.WithContext(ctx))

			var body struct {
				Ticket  string `json:"ticket"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)

			if c.wantTicket {
				if rec.Code != 200 || body.Ticket == "" {
					t.Fatalf("status=%d ticket=%q, want a signed ticket (body=%s)", rec.Code, body.Ticket, rec.Body.String())
				}
				return
			}
			if rec.Code != 403 || body.Ticket != "" {
				t.Fatalf("status=%d ticket=%q, want 403 and no ticket", rec.Code, body.Ticket)
			}
			if body.Message != suspendedMessage {
				t.Errorf("message = %q, want the suspension message so the tenant is told to settle payment", body.Message)
			}
		})
	}
}
