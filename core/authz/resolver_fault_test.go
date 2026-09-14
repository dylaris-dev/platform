package authz

import (
	"database/sql"
	"errors"
	"testing"

	"dylaris-core/models"
)

// faultStore answers one server lookup with a fixed error.
type faultStore struct {
	resolverFakeStore
	err error
}

func (f *faultStore) GetServerByID(int) (*models.Server, error) { return nil, f.err }

// An admin's server rights on a server Core cannot load: a server that does not
// exist grants nothing that exists, so the short-circuit may stay; one that
// cannot be READ must not open, or a database fault opens whatever it is.
func TestResolve_AnAdminAndAServerThatCannotBeLoaded(t *testing.T) {
	foreign := func(int, string) bool { return false }
	id := Identity{UserID: "admin-1", IsAdmin: true}

	r := NewResolver(&faultStore{err: sql.ErrNoRows})
	r.SetForeignNode(foreign)
	if res, _ := r.Resolve(id, 5); !res.HasCap("files.write") {
		t.Error("a server that does not exist changed the admin short-circuit")
	}

	r = NewResolver(&faultStore{err: errors.New("connection reset by peer")})
	r.SetForeignNode(foreign)
	res, _ := r.Resolve(id, 5)
	if res.HasCap("files.write") {
		t.Error("a database fault opened a server to an admin")
	}
	if !res.HasCap("users.write") {
		t.Error("a database fault on one server took an admin's PANEL capabilities")
	}
}
