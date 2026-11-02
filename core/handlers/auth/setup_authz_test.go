package auth

import "testing"

// The rule these decide: who may create an admin through /setup, and when the
// page is allowed to show a form at all.
//
// Two independent gates, and the order matters. SETUP decides whether the door
// exists; ADMIN_SECRET decides who may walk through it. Checking the secret
// first would let a correct secret open a door the operator switched off, which
// is the whole point of adding the switch.

func gate(userCount, adminCount int, setupEnabled bool, configured string) setupGate {
	return setupGate{
		UserCount:        userCount,
		AdminCount:       adminCount,
		SetupEnabled:     setupEnabled,
		SecretConfigured: configured != "",
	}
}

func TestAdminCreateAllowed(t *testing.T) {
	const secret = "correct-horse-battery-staple" // >= 16 chars

	cases := []struct {
		name       string
		userCount  int
		adminCount int
		setup      bool
		configured string
		provided   string
		want       bool
	}{
		// ── No admin exists: SETUP is ignored entirely. ──────────────────────
		// Honouring SETUP=false here would be a lockout with no way in to change
		// it, so a fresh install and a lost-admin instance behave as before.
		{"fresh-install open with SETUP off", 0, 0, false, "", "", true},
		{"fresh-install ignores provided", 0, 0, false, "", "whatever", true},
		{"lost-admin closed without a secret", 3, 0, false, "", "", false},
		{"lost-admin break-glass with SETUP off", 3, 0, false, secret, secret, true},
		{"lost-admin mismatch", 3, 0, false, secret, "wrong", false},
		{"fresh-install secret must still match", 0, 0, false, secret, "wrong", false},
		{"fresh-install secret missing", 0, 0, false, secret, "", false},

		// ── An admin exists: SETUP decides. ──────────────────────────────────
		// This is the case the switch was added for. Before it, a configured
		// ADMIN_SECRET left this door open on every live instance forever.
		{"complete refused while SETUP off, even with the right secret", 1, 1, false, secret, secret, false},
		{"complete allowed while SETUP on with the right secret", 1, 1, true, secret, secret, true},
		{"complete refused while SETUP on with a wrong secret", 1, 1, true, secret, "wrong", false},
		// SETUP on and no secret is not a way in. It is a misconfiguration, and
		// the status endpoint answers it with a warning rather than a form that
		// works.
		{"complete refused while SETUP on with no secret configured", 1, 1, true, "", "", false},
		{"complete refused while SETUP on with no secret and users", 5, 1, true, "", "anything", false},

		// A provided value longer than the configured secret must not match.
		{"longer provided no match", 0, 0, false, secret, secret + "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gate(tc.userCount, tc.adminCount, tc.setup, tc.configured)
			if got := adminCreateAllowed(g, tc.configured, tc.provided); got != tc.want {
				t.Errorf("adminCreateAllowed(users=%d admins=%d setup=%v configured=%t provided=%q) = %v, want %v",
					tc.userCount, tc.adminCount, tc.setup, tc.configured != "", tc.provided, got, tc.want)
			}
		})
	}
}

// The rule this decides: the red banner appears exactly where the missing token
// is about to matter, and nowhere else. Showing it on an instance whose setup is
// switched off would be alarming about a door that is shut.
func TestSetupGateSecretWarning(t *testing.T) {
	cases := []struct {
		name       string
		userCount  int
		adminCount int
		setup      bool
		configured string
		wantOpen   bool
		wantWarn   bool
	}{
		{"fresh install without a secret warns", 0, 0, false, "", true, true},
		{"fresh install with a secret does not", 0, 0, false, "sekrit-sekrit-sekrit", true, false},
		{"lost admin without a secret warns", 4, 0, false, "", true, true},
		{"live instance with setup off is closed and silent", 9, 2, false, "", false, false},
		{"live instance with setup off and a secret is closed and silent", 9, 2, false, "sekrit-sekrit-sekrit", false, false},
		{"live instance with setup on and no secret warns", 9, 2, true, "", true, true},
		{"live instance with setup on and a secret does not", 9, 2, true, "sekrit-sekrit-sekrit", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gate(tc.userCount, tc.adminCount, tc.setup, tc.configured)
			if got := g.Open(); got != tc.wantOpen {
				t.Errorf("Open() = %v, want %v", got, tc.wantOpen)
			}
			if got := g.NeedsSecretWarning(); got != tc.wantWarn {
				t.Errorf("NeedsSecretWarning() = %v, want %v", got, tc.wantWarn)
			}
		})
	}
}
