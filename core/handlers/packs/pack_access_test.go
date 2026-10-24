package packs

import (
	"context"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"
)

type packAccessStore struct {
	store.Store
	rights map[string]store.PackRights // collaborator -> rights on pack 1 (owner "owner")
}

func (f *packAccessStore) GetPack(id int) (*models.Pack, error) {
	return &models.Pack{ID: id, OwnerID: "owner"}, nil
}
func (f *packAccessStore) PackRightsFor(owner, user string, packID int) (store.PackRights, error) {
	return f.rights[user], nil
}

// Who may do what with a pack: the owner everything, an admin reading, a
// collaborator exactly what the owner's invite ticked, anyone else nothing.
func TestPackAccess(t *testing.T) {
	h := NewPacksHandler(&handlers.AppState{Store: &packAccessStore{rights: map[string]store.PackRights{
		"editor":  {Found: true, CanEdit: true},
		"deleter": {Found: true, CanDelete: true},
		"viewer":  {Found: true},
	}}})
	cases := []struct {
		user  string
		admin bool
		want  [3]bool // read, edit, delete
	}{
		{"owner", false, [3]bool{true, true, true}},
		{"admin", true, [3]bool{true, false, false}},
		{"editor", false, [3]bool{true, true, false}},
		{"deleter", false, [3]bool{true, false, true}},
		{"viewer", false, [3]bool{true, false, false}},
		{"stranger", false, [3]bool{false, false, false}},
		{"", false, [3]bool{false, false, false}},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		ctx := context.WithValue(r.Context(), "userID", c.user)
		ctx = context.WithValue(ctx, "isAdmin", c.admin)
		r = r.WithContext(ctx)
		for i, need := range []packNeed{packRead, packEdit, packDelete} {
			if _, got := h.packAccess(r, 1, need); got != c.want[i] {
				t.Errorf("%s need %d: got %v, want %v", c.user, need, got, c.want[i])
			}
		}
	}
}
