package handlers

import (
	"testing"

	"dylaris-core/models"
)

// Deleting a server is a property of the ROLE, not a switch an admin can hand
// out. A hoster's customer cancels, they do not delete, and a paid server
// removed by accident takes its data with it. Support does not get it either:
// looking at a tenant's server is the job, removing it is not.
//
// Both enforcement sites read EffectivePermissions.CanDeleteServers
// (servers_lifecycle.go, server and sub-server delete), so this is the one
// place the answer has to be right.
func TestDeletingServersIsAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		user models.User
		want bool
	}{
		{"admin by flag", models.User{IsAdmin: true}, true},
		{"admin by role", models.User{Role: "admin"}, true},
		{
			// The case that changed: the stored flag used to be enough.
			name: "user with the stored flag still set",
			user: models.User{Role: "user", CanDeleteServers: true},
			want: false,
		},
		{
			name: "support with the stored flag still set",
			user: models.User{Role: "support", CanDeleteServers: true},
			want: false,
		},
		{"plain user", models.User{Role: "user"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.user
			got := ComputeEffectivePermissions(&u).CanDeleteServers
			if got != tc.want {
				t.Errorf("CanDeleteServers = %v, want %v", got, tc.want)
			}
		})
	}
}

// Changing resources is NOT admin-only: a customer may resize what they own.
// The scoping is done by the capability on the route, which the resolver
// evaluates against the one server in the request - so this flag answers
// "at all", and ownership answers "where".
func TestChangingResourcesStaysAvailableToNonAdmins(t *testing.T) {
	u := models.User{Role: "user", CanChangeResources: true}
	p := ComputeEffectivePermissions(&u)
	if !p.CanChangeResources {
		t.Error("a user granted the resource flag lost it")
	}
	if p.CanDeleteServers {
		t.Error("the resource flag must not carry the delete right with it")
	}
}
