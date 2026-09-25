package handlers

import (
	"testing"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// ticketCapStore answers the three calls LoadEffectivePermissions and the
// resolver make for a plain user.
type ticketCapStore struct {
	store.Store
	user *models.User
	role *store.PanelRole
}

func (f *ticketCapStore) GetUserByID(id string) (*models.User, error) { return f.user, nil }
func (f *ticketCapStore) GetUserRegionIDs(id string) ([]string, error) {
	return nil, nil
}
func (f *ticketCapStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	if f.role == nil {
		return nil, store.CapOverrides{}, nil
	}
	id := f.role.ID
	return &id, store.CapOverrides{}, nil
}
func (f *ticketCapStore) GetPanelRole(id int) (*store.PanelRole, error) { return f.role, nil }

func permsFor(t *testing.T, user *models.User, role *store.PanelRole) EffectivePermissions {
	t.Helper()
	st := &ticketCapStore{user: user, role: role}
	return LoadEffectivePermissions(&AppState{Store: st, Authz: authz.NewResolver(st)}, user.ID)
}

// The platform has TWO ways to make somebody support, and the ticket subsystem
// only ever honoured the older one.
//
// Measured on production: an account holding the seeded "support" PANEL role -
// which carries tickets.read and tickets.write, is what the panel's role screen
// offers, and passes RequireCap on the route - got 403 reading a customer's
// ticket, an empty inbox, and no notification that the ticket existed. Setting
// the legacy users.role column to "support" on the same account fixed all
// three, which is what pinned the cause.
func TestPanelRoleMakesSomebodySupport(t *testing.T) {
	supportRole := &store.PanelRole{ID: 2, Name: "support", Capabilities: []string{
		"tickets.read", "tickets.write", "users.read", "servers.read", "audit.read",
	}}

	t.Run("the support panel role alone is enough", func(t *testing.T) {
		p := permsFor(t, &models.User{ID: "u1", Role: "user"}, supportRole)
		if !p.IsSupport || !p.CanManageTickets {
			t.Fatalf("IsSupport=%v CanManageTickets=%v; the role the panel hands out must work",
				p.IsSupport, p.CanManageTickets)
		}
		if p.IsAdmin {
			t.Error("IsAdmin = true; support is not an admin")
		}
	})

	t.Run("tickets.read alone sees the queue but does not act on it", func(t *testing.T) {
		readOnly := &store.PanelRole{ID: 3, Name: "triage", Capabilities: []string{"tickets.read"}}
		p := permsFor(t, &models.User{ID: "u2", Role: "user"}, readOnly)
		if !p.IsSupport {
			t.Error("IsSupport = false; tickets.read is what seeing the queue means")
		}
		if p.CanManageTickets {
			t.Error("CanManageTickets = true without tickets.write; a reader must not answer in the platform's name")
		}
	})

	t.Run("the legacy role still works on its own", func(t *testing.T) {
		p := permsFor(t, &models.User{ID: "u3", Role: "support"}, nil)
		if !p.IsSupport || !p.CanManageTickets {
			t.Fatalf("IsSupport=%v CanManageTickets=%v; the old mechanism must keep working", p.IsSupport, p.CanManageTickets)
		}
	})

	t.Run("an ordinary user is neither", func(t *testing.T) {
		p := permsFor(t, &models.User{ID: "u4", Role: "user"}, nil)
		if p.IsSupport || p.CanManageTickets {
			t.Fatalf("IsSupport=%v CanManageTickets=%v", p.IsSupport, p.CanManageTickets)
		}
	})
}
