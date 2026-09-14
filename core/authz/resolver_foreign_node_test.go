package authz

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// An admin opens every server on the platform's machines and none on a
// customer's, except through that customer's own invite. Server ids are
// sequential, so the list hiding the server was never the fence; this is.
func TestResolve_AnAdminOnACustomersMachine(t *testing.T) {
	const (
		admin    = "admin-1"
		customer = "customer-1"
		ours     = 1 // on node 10, the platform's
		theirs   = 2 // on node 20, the customer's
	)
	st := &resolverFakeStore{
		servers: map[int]*models.Server{
			ours:   {ID: ours, OwnerID: customer, NodeID: 10},
			theirs: {ID: theirs, OwnerID: customer, NodeID: 20},
		},
	}
	foreign := func(nodeID int, userID string) bool { return nodeID == 20 && userID != customer }
	id := Identity{UserID: admin, IsAdmin: true}

	t.Run("a platform machine is still everything", func(t *testing.T) {
		r := NewResolver(st)
		r.SetForeignNode(foreign)
		res, _ := r.Resolve(id, ours)
		if !res.HasCap("files.write") || !res.HasCap("users.write") {
			t.Error("an admin lost rights on a server on the platform's own machine")
		}
	})

	t.Run("a customer's machine grants no server rights", func(t *testing.T) {
		r := NewResolver(st)
		r.SetForeignNode(foreign)
		res, _ := r.Resolve(id, theirs)
		for _, c := range []string{"files.read", "files.write", "console.read", "sftp.access"} {
			if res.HasCap(c) {
				t.Errorf("an admin holds %s on a customer's machine", c)
			}
		}
		if res.HasAnyServerCap() {
			t.Error("the server would still appear in the admin's list")
		}
		// Still an admin of the panel: the fence is about the machine, not the account.
		if !res.HasCap("users.write") {
			t.Error("an admin lost a PANEL capability by looking at a customer's server")
		}
	})

	t.Run("the customer's invite still works, with its own scope", func(t *testing.T) {
		sid := theirs
		invited := &resolverFakeStore{
			servers: st.servers,
			serverGrants: map[string]*store.ServerGrant{
				gkey(theirs, admin): {ServerID: &sid, UserID: admin, CapOverrides: store.CapOverrides{Grant: []string{"console.read"}}},
			},
		}
		r := NewResolver(invited)
		r.SetForeignNode(foreign)
		res, _ := r.Resolve(id, theirs)
		if !res.HasCap("console.read") {
			t.Error("the customer invited the admin to their console and the invite did not work")
		}
		if res.HasCap("files.write") {
			t.Error("the invite granted the console only, and the admin flag widened it")
		}
	})

	t.Run("without the hook nothing changes", func(t *testing.T) {
		res, _ := NewResolver(st).Resolve(id, theirs)
		if !res.HasCap("files.write") {
			t.Error("a resolver with no foreign-node predicate must keep the admin short-circuit")
		}
	})
}
