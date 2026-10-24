package tickets

import (
	"database/sql"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// ticketAttachFakeStore is the smallest store the resolver plus
// LoadEffectivePermissions need: a server by uuid and by id, the user row, and
// the three grant reads Resolve makes.
type ticketAttachFakeStore struct {
	store.Store
	servers map[string]*models.Server
	users   map[string]*models.User
	grants  map[string][]string // userID -> capabilities on the server
}

func (f *ticketAttachFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	if s, ok := f.servers[uuid]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}

func (f *ticketAttachFakeStore) GetServerByID(id int) (*models.Server, error) {
	for _, s := range f.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (f *ticketAttachFakeStore) GetUserByID(id string) (*models.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, sql.ErrNoRows
}

func (f *ticketAttachFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	return nil, store.CapOverrides{}, nil
}

func (f *ticketAttachFakeStore) GetInvite(serverID int, userID string) (*models.ServerInvite, error) {
	return nil, sql.ErrNoRows
}

func (f *ticketAttachFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if caps, ok := f.grants[userID]; ok {
		return &store.ServerGrant{
			ServerID:     &serverID,
			UserID:       userID,
			CapOverrides: store.CapOverrides{Grant: caps},
		}, nil
	}
	return nil, nil
}

func (f *ticketAttachFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	return nil, nil
}

// A ticket could name ANY server uuid, and Core answered with that server's
// NAME and internal id - an existence-and-name oracle for anyone holding a
// uuid, plus a row that routes the server into a supporter's sidebar through
// ListServersViaActiveTickets. The refusal must be identical for "no such
// server" and "not yours", or the oracle survives with an extra step.
func TestMayAttachServer(t *testing.T) {
	const ownerID = "owner-id"
	const strangerID = "stranger-id"
	const granteeID = "grantee-id"
	const adminID = "admin-id"

	fs := &ticketAttachFakeStore{
		servers: map[string]*models.Server{
			"srv-uuid": {ID: 7, UUID: "srv-uuid", OwnerID: ownerID, OwnerName: "owner", Region: "eu"},
		},
		users: map[string]*models.User{
			ownerID:    {ID: ownerID, Username: "owner"},
			strangerID: {ID: strangerID, Username: "stranger"},
			granteeID:  {ID: granteeID, Username: "grantee"},
			adminID:    {ID: adminID, Username: "admin", IsAdmin: true},
		},
		grants: map[string][]string{
			granteeID: {"overview.read"},
		},
	}
	h := &TicketsHandler{state: &handlers.AppState{Store: fs, Authz: authz.NewResolver(fs)}}

	cases := []struct {
		name     string
		userID   string
		username string
		uuid     string
		want     bool
	}{
		{"the owner may name their own server", ownerID, "owner", "srv-uuid", true},
		{"an admin may name any server", adminID, "admin", "srv-uuid", true},
		{"a grantee who can open it may name it", granteeID, "grantee", "srv-uuid", true},
		{"a stranger may not", strangerID, "stranger", "srv-uuid", false},
		{"a uuid that does not exist", ownerID, "owner", "no-such-uuid", false},
		{"an empty uuid never reaches here, but must not pass", ownerID, "owner", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			region, got := h.mayAttachServer(c.userID, c.username, c.uuid)
			if got != c.want {
				t.Errorf("mayAttachServer(%q, %q) = %v, want %v", c.username, c.uuid, got, c.want)
			}
			// The region is the reason this returns anything besides a bool: the
			// ticket records where the server is, and it must come from the row
			// that was just authorised rather than from the request.
			wantRegion := ""
			if c.want {
				wantRegion = "eu"
			}
			if region != wantRegion {
				t.Errorf("region = %q, want %q", region, wantRegion)
			}
		})
	}

	t.Run("a stranger and an unknown uuid are indistinguishable", func(t *testing.T) {
		// Both false is the whole point: the caller turns either into the same
		// "Server not found", so no answer here may leak which case it was.
		strangerRegion, stranger := h.mayAttachServer(strangerID, "stranger", "srv-uuid")
		unknownRegion, unknown := h.mayAttachServer(strangerID, "stranger", "no-such-uuid")
		if stranger != unknown {
			t.Errorf("stranger=%v unknown=%v - the two must be the same answer", stranger, unknown)
		}
		// The region must not leak either: a non-empty one for the real uuid
		// would tell a stranger the server exists.
		if strangerRegion != unknownRegion {
			t.Errorf("region stranger=%q unknown=%q - a refusal must not say the server exists", strangerRegion, unknownRegion)
		}
	})
}
