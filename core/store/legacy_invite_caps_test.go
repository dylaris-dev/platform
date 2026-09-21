package store

import (
	"reflect"
	"testing"

	"dylaris-core/models"
)

// The roster answers "who may do what on my server", and it was answering it
// from the legacy blob alone. A member added through POST /api/grants - the
// route the panel's Access page uses - has an empty blob and real capabilities,
// so the roster printed every flag false for somebody holding full server
// admin. Measured on production with an account holding the operator set.
func TestTabPermissionsFromCaps(t *testing.T) {
	cases := []struct {
		name string
		caps []string
		want models.TabPermissions
	}{
		{
			name: "no capabilities is no tabs",
			caps: nil,
			want: models.TabPermissions{},
		},
		{
			name: "the operator set: console, power and players, nothing else",
			caps: []string{
				"overview.read", "stats.read", "console.read", "console.send",
				"power.start", "power.stop", "power.restart",
				"rcon.exec", "players.read", "players.manage",
			},
			want: models.TabPermissions{
				Overview: true, Console: true, Power: true, Players: true,
			},
		},
		{
			// The read cap is what opens the tab: a member who may write files
			// but not read them is not a shape any grant produces, and writing
			// alone would show an empty browser.
			name: "files needs the read cap",
			caps: []string{"files.write", "files.delete"},
			want: models.TabPermissions{},
		},
		{
			name: "any one power capability lights the tab",
			caps: []string{"power.kill"},
			want: models.TabPermissions{Power: true},
		},
		{
			name: "setup is server.settings.write",
			caps: []string{"server.settings.write"},
			want: models.TabPermissions{Setup: true},
		},
		{
			name: "the sensitive reads each have their own tab",
			caps: []string{"members.read", "network.read", "backups.read", "config.read"},
			want: models.TabPermissions{Members: true, Network: true, Backups: true, Config: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TabPermissionsFromCaps(c.caps); got != c.want {
				t.Errorf("TabPermissionsFromCaps = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Inherit is not a capability. It lives in its own column and the caller sets
// it, so this must never invent one.
func TestTabPermissionsFromCapsNeverSetsInherit(t *testing.T) {
	if TabPermissionsFromCaps([]string{"overview.read", "inherit"}).Inherit {
		t.Error("inherit came out of the capability set; it is carried by the invite column")
	}
}

// A legacy invite and a grant have to read the same, or the roster would
// report two members with identical access differently depending on which
// route added them.
func TestTabPermissionsFromCapsAgreesWithTheLegacyMapping(t *testing.T) {
	legacy := models.TabPermissions{Console: true, Power: true, Overview: true}
	got := TabPermissionsFromCaps(MapLegacyInviteCaps(legacy))

	want := legacy
	// Legacy "power" also conferred players.read, so the Players tab is one
	// that member has had all along; the blob simply never recorded it.
	want.Players = true
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestEffectiveGrantCaps(t *testing.T) {
	cases := []struct {
		name     string
		roleCaps []string
		ov       CapOverrides
		want     []string
	}{
		{"nothing at all", nil, CapOverrides{}, []string{}},
		{
			name: "overrides alone, sorted so a roster row is stable",
			ov:   CapOverrides{Grant: []string{"console.send", "console.read"}},
			want: []string{"console.read", "console.send"},
		},
		{
			name:     "a role plus an override",
			roleCaps: []string{"overview.read"},
			ov:       CapOverrides{Grant: []string{"console.read"}},
			want:     []string{"console.read", "overview.read"},
		},
		{
			name:     "deny wins over both, like applyGrant",
			roleCaps: []string{"overview.read", "power.kill"},
			ov:       CapOverrides{Grant: []string{"console.read"}, Deny: []string{"power.kill", "console.read"}},
			want:     []string{"overview.read"},
		},
		{
			name:     "a duplicate is one capability",
			roleCaps: []string{"console.read"},
			ov:       CapOverrides{Grant: []string{"console.read"}},
			want:     []string{"console.read"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveGrantCaps(c.roleCaps, c.ov); !reflect.DeepEqual(got, c.want) {
				t.Errorf("EffectiveGrantCaps = %v, want %v", got, c.want)
			}
		})
	}
}
