package authz

import "strings"

// neverOnAKey are capabilities no key may hold even though their scope would
// allow it: managing keys, roles or members hands out access, and a credential
// that can mint or widen credentials outlives every check made when it was
// minted. library.* belongs to a removed feature.
var neverOnAKey = []string{"apikeys.", "roles.", "members.write", "members.delete", "library."}

// ValidKeyCap reports whether capID may be carried by an API key: it must be a
// known catalog capability whose scope is SERVER or OWNER, and not one that
// manages access. PANEL caps (staff operations) and unknown ids are rejected -
// a key is an owner-scoped automation credential, not a staff principal.
func ValidKeyCap(capID string) bool {
	c, ok := Get(capID)
	if !ok || (c.Scope != ScopeServer && c.Scope != ScopeOwner) {
		return false
	}
	for _, p := range neverOnAKey {
		if strings.HasPrefix(capID, p) {
			return false
		}
	}
	return true
}

// KeyCaps lists every capability a key may carry, in catalog order.
func KeyCaps() []string {
	var out []string
	for _, c := range All() {
		if ValidKeyCap(c.ID) {
			out = append(out, c.ID)
		}
	}
	return out
}

// ResolveAPIKey builds a Resolution representing an API key's authority: the
// key's SERVER caps (only when serverAllowed, i.e. the key's server scope covers
// the addressed server) plus its OWNER caps. It has no admin/owner short-
// circuit and no panel caps - a key holds exactly the capabilities it was
// minted with. Routing key checks through this + HasCap unifies key auth with
// session auth on the one decision function.
//
// Note: ownerCaps here are not bound to a specific owner realm, so every
// OWNER-scoped key route MUST bind that realm to the key's UserID (the
// creator) itself, or an owner-cap key could act on another owner's realm.
// The two that exist do: handlers.ExternalOwnerRoute rewrites the request
// identity to the key owner before the session handler runs, and
// ListExternalServers queries by the key's UserID directly. Anything new wired
// to APIKeyOwnerRoute has to do the same - this function cannot do it for you,
// because it never sees the key.
func ResolveAPIKey(perms []string, serverAllowed bool) *Resolution {
	res := &Resolution{
		panelCaps:  map[string]bool{},
		serverCaps: map[string]bool{},
		ownerCaps:  map[string]bool{},
	}
	for _, capID := range perms {
		c, ok := Get(capID)
		// Checked at use as well as at mint: a key minted before a capability
		// was put out of reach must not keep it.
		if !ok || !ValidKeyCap(capID) {
			continue
		}
		switch c.Scope {
		case ScopeServer:
			if serverAllowed {
				res.serverCaps[capID] = true
			}
		case ScopeOwner:
			res.ownerCaps[capID] = true
		}
	}
	return res
}
