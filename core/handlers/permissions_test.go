package handlers

import (
	"errors"
	"reflect"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// permissionsFakeStore embeds store.Store (nil) so it satisfies the full
// interface at compile time; only the methods LoadEffectivePermissions
// touches are overridden. Any other call would panic - these tests never
// make one.
type permissionsFakeStore struct {
	store.Store
	user    *models.User
	userErr error
}

func (f *permissionsFakeStore) GetUserByID(string) (*models.User, error) { return f.user, f.userErr }

func TestComputeEffectivePermissions(t *testing.T) {
	cases := []struct {
		name string
		user *models.User
		want EffectivePermissions
	}{
		{
			name: "nil user denies everything by default",
			user: nil,
			want: EffectivePermissions{Role: "user"},
		},
		{
			name: "is_admin flag grants everything regardless of per-user flags",
			user: &models.User{IsAdmin: true, Role: "user", CanDeleteServers: false, CanChangeResources: false},
			want: EffectivePermissions{
				Role: "admin", IsAdmin: true, IsSupport: true, CanManageTickets: true,
				CanDeleteServers: true, CanChangeResources: true,
			},
		},
		{
			name: "role=admin grants everything even without the is_admin flag",
			user: &models.User{IsAdmin: false, Role: "admin"},
			want: EffectivePermissions{
				Role: "admin", IsAdmin: true, IsSupport: true, CanManageTickets: true,
				CanDeleteServers: true, CanChangeResources: true,
			},
		},
		{
			name: "empty role defaults to user",
			user: &models.User{Role: ""},
			want: EffectivePermissions{Role: "user"},
		},
		{
			// The LEGACY role keeps conferring both halves. The capability half is
			// resolved by LoadEffectivePermissions, which this pure function does
			// not have, so a user with no legacy role comes out with neither -
			// exactly the behaviour that existed before the split.
			name: "support role is flagged as support and may manage, not admin",
			user: &models.User{Role: "support"},
			want: EffectivePermissions{Role: "support", IsSupport: true, CanManageTickets: true},
		},
		{
			// The stored can_delete_servers is deliberately NOT carried through:
			// deleting is admin-only and follows the role. The resource flag
			// still is - see TestDeletingServersIsAdminOnly.
			name: "a regular user's stored delete flag is ignored, the rest carries through",
			user: &models.User{Role: "user", CanDeleteServers: true, CanChangeResources: true},
			want: EffectivePermissions{Role: "user", CanDeleteServers: false, CanChangeResources: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeEffectivePermissions(tc.user)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeEffectivePermissions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadEffectivePermissions(t *testing.T) {
	t.Run("nil state denies by default", func(t *testing.T) {
		got := LoadEffectivePermissions(nil, "u1")
		if !reflect.DeepEqual(got, EffectivePermissions{Role: "user"}) {
			t.Fatalf("got %+v, want zero-value user", got)
		}
	})

	t.Run("nil store denies by default", func(t *testing.T) {
		got := LoadEffectivePermissions(&AppState{}, "u1")
		if !reflect.DeepEqual(got, EffectivePermissions{Role: "user"}) {
			t.Fatalf("got %+v, want zero-value user", got)
		}
	})

	t.Run("empty userID denies by default", func(t *testing.T) {
		fs := &permissionsFakeStore{}
		got := LoadEffectivePermissions(&AppState{Store: fs}, "")
		if !reflect.DeepEqual(got, EffectivePermissions{Role: "user"}) {
			t.Fatalf("got %+v, want zero-value user", got)
		}
	})

	t.Run("a failed user lookup denies by default", func(t *testing.T) {
		fs := &permissionsFakeStore{userErr: errors.New("db down")}
		got := LoadEffectivePermissions(&AppState{Store: fs}, "u1")
		if !reflect.DeepEqual(got, EffectivePermissions{Role: "user"}) {
			t.Fatalf("got %+v, want zero-value user", got)
		}
	})

	t.Run("a nil user (no error) denies by default", func(t *testing.T) {
		fs := &permissionsFakeStore{user: nil}
		got := LoadEffectivePermissions(&AppState{Store: fs}, "u1")
		if !reflect.DeepEqual(got, EffectivePermissions{Role: "user"}) {
			t.Fatalf("got %+v, want zero-value user", got)
		}
	})

	t.Run("success path computes from the user row", func(t *testing.T) {
		// CanChangeResources rather than CanDeleteServers, because the delete
		// flag no longer survives the computation for a non-admin and would
		// make this assert nothing.
		fs := &permissionsFakeStore{
			user: &models.User{Role: "user", CanChangeResources: true},
		}
		got := LoadEffectivePermissions(&AppState{Store: fs}, "u1")
		want := EffectivePermissions{Role: "user", CanChangeResources: true}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}
