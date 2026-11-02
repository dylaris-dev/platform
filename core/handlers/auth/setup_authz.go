package auth

import (
	"crypto/sha256"
	"crypto/subtle"
)

// setupGate is everything the /setup decisions need, in one value so the two
// callers (the status endpoint and the create endpoint) cannot answer from
// different facts. A page that says the door is open while the handler refuses
// is worse than either being wrong on its own.
type setupGate struct {
	UserCount  int
	AdminCount int
	// SetupEnabled is env SETUP. Only consulted once an admin exists.
	SetupEnabled bool
	// SecretConfigured is ADMIN_SECRET being non-empty.
	SecretConfigured bool
}

// Open reports whether /setup should present a working form at all.
//
//   - No admin exists: always open, whatever SETUP says. This is a fresh install
//     or an instance that lost every admin, and honouring SETUP=false here would
//     be a lockout nobody could unlock from inside.
//   - An admin exists: open only when SETUP is true. The secret is checked
//     separately, on submit.
func (g setupGate) Open() bool {
	if g.AdminCount == 0 {
		return true
	}
	return g.SetupEnabled
}

// NeedsSecretWarning reports whether /setup must show that no admin token is
// configured. It fires exactly where the missing token is about to matter:
// the door is open, and without ADMIN_SECRET nothing can come through it
// except a pristine first install.
//
// Without this the operator sees a form, fills it in, and is refused with no
// way to tell that the cause is an env var they never set.
func (g setupGate) NeedsSecretWarning() bool {
	return !g.SecretConfigured && g.Open()
}

// adminCreateAllowed decides whether POST /api/setup/admin may create an admin.
// provided is the secret from the request body.
//
//   - Closed by SETUP: refused outright. Checked first, so a correct secret does
//     not open a door the operator switched off.
//   - No secret configured: only a pristine fresh install (userCount == 0) is
//     open. An instance that already has users and no secret has no recovery,
//     deliberately - the alternative is anonymous admin creation on a live
//     system.
//   - Secret configured: allowed in any open mode iff the provided secret
//     matches, compared over SHA-256 digests so unequal input lengths cannot
//     leak via timing.
func adminCreateAllowed(g setupGate, configured, provided string) bool {
	if !g.Open() {
		return false
	}
	if configured == "" {
		return g.UserCount == 0
	}
	p := sha256.Sum256([]byte(provided))
	c := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(p[:], c[:]) == 1
}
