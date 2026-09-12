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
	user       *models.User
	userErr    error
	regions    []string
	regionsErr error
}

func (f *permissionsFakeStore) GetUserByID(string) (*models.User, error) { return f.user, f.userErr }
func (f *permissionsFakeStore) GetUserRegionIDs(string) ([]string, error) {
	return f.regions, f.regionsErr
}

func TestComputeEffectivePermissions(t *testing.T) {
	cases := []struct {
		name           string
		user           *models.User
		allowedRegions []string
		want           EffectivePermissions
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
				Role: "admin", IsAdmin: true, CanAccessAllRegions: true,
				CanDeleteServers: true, CanChangeResources: true,
			},
		},
		{
			name: "role=admin grants everything even without the is_admin flag",
			user: &models.User{IsAdmin: false, Role: "admin"},
			want: EffectivePermissions{
				Role: "admin", IsAdmin: true, CanAccessAllRegions: true,
				CanDeleteServers: true, CanChangeResources: true,
			},
		},
		{
			name: "empty role defaults to user",
			user: &models.User{Role: ""},
			want: EffectivePermissions{Role: "user"},
		},
		{
			name: "support role is flagged as support, not admin",
			user: &models.User{Role: "support"},
			want: EffectivePermissions{Role: "support", IsSupport: true},
		},
		{
			// The stored can_delete_servers is deliberately NOT carried through:
			// deleting is admin-only and follows the role. The resource flag and
			// the region list still are - see TestDeletingServersIsAdminOnly.
			name:           "a regular user's stored delete flag is ignored, the rest carries through",
			user:           &models.User{Role: "user", CanDeleteServers: true, CanChangeResources: true, AllRegionsAccess: false},
			allowedRegions: []string{"eu", "us"},
			want: EffectivePermissions{
				Role: "user", CanDeleteServers: false, CanChangeResources: true,
				CanAccessAllRegions: false, AllowedRegions: []string{"eu", "us"},
			},
		},
		{
			name: "AllRegionsAccess grants all-regions without being admin",
			user: &models.User{Role: "user", AllRegionsAccess: true},
			want: EffectivePermissions{Role: "user", CanAccessAllRegions: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeEffectivePermissions(tc.user, tc.allowedRegions)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeEffectivePermissions = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCanAccessRegion(t *testing.T) {
	cases := []struct {
		name   string
		perms  EffectivePermissions
		region string
		want   bool
	}{
		{"all-regions always passes, even an empty region", EffectivePermissions{CanAccessAllRegions: true}, "", true},
		{"all-regions passes for any specific region", EffectivePermissions{CanAccessAllRegions: true}, "eu", true},
		{"explicit region membership matches", EffectivePermissions{AllowedRegions: []string{"eu", "us"}}, "eu", true},
		{"explicit region membership rejects a non-member", EffectivePermissions{AllowedRegions: []string{"eu"}}, "us", false},
		{"empty region defensively maps to default and matches", EffectivePermissions{AllowedRegions: []string{"default"}}, "", true},
		{"empty region maps to default but is denied without it", EffectivePermissions{AllowedRegions: []string{"eu"}}, "", false},
		{"no allowed regions denies everything", EffectivePermissions{}, "eu", false},
		// Case- and space-insensitive, matching services.equalRegion and
		// PickBeamRelay. This was the one exact-match region comparison, and the
		// one where a mismatch is silent - it hides a server from the staff
		// member meant to see it instead of falling back like the other two.
		{"a region differing only in case still matches", EffectivePermissions{AllowedRegions: []string{"eu"}}, "EU", true},
		{"a stored region with surrounding space still matches", EffectivePermissions{AllowedRegions: []string{" eu "}}, "eu", true},
		{"folding does not turn a non-member into a member", EffectivePermissions{AllowedRegions: []string{"eu"}}, "US", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.perms.CanAccessRegion(tc.region); got != tc.want {
				t.Errorf("CanAccessRegion(%q) = %v, want %v", tc.region, got, tc.want)
			}
		})
	}
}

func TestFilterServersByRegion(t *testing.T) {
	servers := []models.Server{
		{UUID: "s-eu", Region: "eu"},
		{UUID: "s-us", Region: "us"},
		{UUID: "s-default", Region: ""},
	}

	t.Run("all-regions returns every server unfiltered", func(t *testing.T) {
		got := FilterServersByRegion(servers, EffectivePermissions{CanAccessAllRegions: true}, "staff")
		if len(got) != len(servers) {
			t.Fatalf("got %d servers, want %d", len(got), len(servers))
		}
	})

	t.Run("explicit regions filter out inaccessible servers, defaulting empty region to default", func(t *testing.T) {
		got := FilterServersByRegion(servers, EffectivePermissions{AllowedRegions: []string{"eu", "default"}}, "staff")
		if len(got) != 2 {
			t.Fatalf("got %d servers, want 2: %+v", len(got), got)
		}
		for _, s := range got {
			if s.UUID != "s-eu" && s.UUID != "s-default" {
				t.Errorf("unexpected server in result: %+v", s)
			}
		}
	})

	t.Run("no allowed regions filters everything out", func(t *testing.T) {
		got := FilterServersByRegion(servers, EffectivePermissions{}, "staff")
		if len(got) != 0 {
			t.Fatalf("got %d servers, want 0: %+v", len(got), got)
		}
	})
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

	t.Run("success path composes the user and their region IDs", func(t *testing.T) {
		// CanChangeResources rather than CanDeleteServers, because the delete
		// flag no longer survives the computation for a non-admin and would
		// make this assert nothing.
		fs := &permissionsFakeStore{
			user:    &models.User{Role: "user", CanChangeResources: true},
			regions: []string{"eu", "us"},
		}
		got := LoadEffectivePermissions(&AppState{Store: fs}, "u1")
		want := EffectivePermissions{Role: "user", CanChangeResources: true, AllowedRegions: []string{"eu", "us"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

// The rule the region filter did NOT have, and the one this platform actually
// needs: a region set never hides a server from the person who owns it.
//
// Regions answer "which regions may this STAFF member look at". A customer's set
// is empty unless an admin filled it in, and a self-registered account gets none
// by default - so the filter dropped every server such an account had, its own
// included. Measured during BYON testing: the server was created, it ran, the
// admin could see it, and its owner got an empty list.
func TestFilterServersByRegionNeverHidesYourOwnServer(t *testing.T) {
	servers := []models.Server{
		{UUID: "mine", Region: "us", OwnerID: "tenant-1"},
		{UUID: "someone-elses", Region: "us", OwnerID: "tenant-2"},
	}

	t.Run("an owner with no regions at all still sees their own", func(t *testing.T) {
		got := FilterServersByRegion(servers, EffectivePermissions{}, "tenant-1")
		if len(got) != 1 || got[0].UUID != "mine" {
			t.Fatalf("got %+v, want only the owned server", got)
		}
	})

	// And it must not become a way around the filter: ownership widens the view
	// by exactly the servers you own, nothing more.
	t.Run("it does not hand over anyone else's", func(t *testing.T) {
		got := FilterServersByRegion(servers, EffectivePermissions{AllowedRegions: []string{"eu"}}, "tenant-1")
		for _, s := range got {
			if s.OwnerID != "tenant-1" {
				t.Errorf("a server owned by %q leaked through: %+v", s.OwnerID, s)
			}
		}
	})

	// No identity means no ownership exemption - a path that cannot name its
	// caller keeps the old, narrower behaviour rather than silently widening.
	t.Run("an anonymous viewer is filtered as before", func(t *testing.T) {
		if got := FilterServersByRegion(servers, EffectivePermissions{}, ""); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})
}

// The same rule one step further out: an invitation is the owner saying "this
// person may use my server", and a staff region set does not get a say in it.
// The invitee could open the server by URL while their own list left it out.
func TestFilterServersByRegionNeverHidesAServerYouWereInvitedTo(t *testing.T) {
	servers := []models.Server{
		{UUID: "invited", Region: "us", OwnerID: "owner-a", Role: "invited"},
		{UUID: "inherited", Region: "us", OwnerID: "owner-a", Role: "inherited"},
		// A row with no role came from somewhere other than this viewer's own
		// list, and must be filtered exactly as before.
		{UUID: "not-yours", Region: "us", OwnerID: "owner-b"},
	}

	got := FilterServersByRegion(servers, EffectivePermissions{AllowedRegions: []string{"eu"}}, "friend")
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.UUID] = true
	}
	if !seen["invited"] || !seen["inherited"] {
		t.Fatalf("got %+v, want both the invited and the inherited server kept", got)
	}
	if seen["not-yours"] {
		t.Fatalf("a server with no role leaked through the region filter: %+v", got)
	}

	t.Run("an anonymous viewer gets no invitation exemption either", func(t *testing.T) {
		if got := FilterServersByRegion(servers, EffectivePermissions{}, ""); len(got) != 0 {
			t.Fatalf("got %+v, want none", got)
		}
	})
}
