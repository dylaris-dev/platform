package authz

import (
	"context"

	"dylaris-core/store"
)

// Identity is the request principal, extracted from the context keys that
// AuthMiddleware sets. An admin short-circuits every capability check.
type Identity struct {
	UserID   string
	Username string
	IsAdmin  bool
}

// IdentityFromContext reads the "userID"/"username"/"isAdmin" context values
// set by handlers.AuthHandler.AuthMiddleware. Missing values yield zero fields.
func IdentityFromContext(ctx context.Context) Identity {
	id := Identity{}
	if v, ok := ctx.Value("userID").(string); ok {
		id.UserID = v
	}
	if v, ok := ctx.Value("username").(string); ok {
		id.Username = v
	}
	if v, ok := ctx.Value("isAdmin").(bool); ok {
		id.IsAdmin = v
	}
	return id
}

// Resolver materializes effective capabilities for a request. It is the single
// authorization chokepoint that subsumes the legacy checkServerAccess / inline
// IsAdmin / EffectivePermissions paths (wired into routes in phase 2).
type Resolver struct {
	store    Store
	demoRead func(serverID int) bool // optional; StoreEnabled-gated by the caller
}

func NewResolver(st Store) *Resolver {
	return &Resolver{store: st}
}

// SetDemoRead installs an optional predicate that reports whether a server is an
// admin-flagged public read-only showcase. nil-safe: unset means no demo access.
func (r *Resolver) SetDemoRead(fn func(serverID int) bool) { r.demoRead = fn }

// Resolution is the materialized decision context for one (identity, scope).
// It is deny-by-default: a capability is granted only when a short-circuit or
// an explicit resolved cap covers it. HasCap answers individual checks.
type Resolution struct {
	admin      bool            // panel admin: holds every capability
	ownerSelf  bool            // owns the realm in scope (own server, or serverID==0 self-realm)
	demoRead   bool            // server is an admin-flagged demo showcase: grant SERVER read caps to any authed principal
	panelCaps  map[string]bool // PANEL caps from panel role + per-user overrides
	serverCaps map[string]bool // SERVER caps from direct/proxy/account grant
	ownerCaps  map[string]bool // OWNER caps from an account grant on this realm
}

// demoReadDeny lists SERVER read caps that a public demo showcase must NOT grant
// to arbitrary authenticated viewers even though they are reads: network.read
// discloses routing/endpoint info (topology, relevant to node-IP obfuscation) and
// members.read discloses the access roster. Any future sensitive server read cap
// must be added here so it is not auto-exposed on public demo servers.
var demoReadDeny = map[string]bool{
	"network.read": true,
	"members.read": true,
	// files.read is handled by the file browser handler itself (it applies demo
	// content redaction via viaDemoBypass); the resolver must NOT grant it on a
	// demo server or redaction would be skipped for demo viewers.
	"files.read": true,
	// server.audit.read discloses the IP address and user agent of the owner and
	// of every member, which is the sharpest disclosure on this list - a public
	// showcase must never hand that to an arbitrary authenticated viewer.
	"server.audit.read": true,
}

// HasCap reports whether the resolution grants capID. An unknown capability is
// always denied. Admin grants everything. Otherwise the cap's catalog scope
// selects which resolved set (plus the owner short-circuit) is consulted.
//
// Invariant (guaranteed by RequireCap): SERVER caps are only ever checked with
// a resolution built for a concrete server, so ownerSelf there means "owns THAT
// server"; OWNER caps are checked with serverID==0, so ownerSelf means "own
// realm". A single ownerSelf flag is therefore correct for both.
func (res *Resolution) HasCap(capID string) bool {
	c, ok := Get(capID)
	if !ok {
		return false
	}
	if res.admin {
		return true
	}
	// Demo showcase: any authenticated viewer may READ a demo server's operational
	// state (overview, console, stats, config, mods, tabs, schedule, ...).
	// network.read, members.read, and files.read stay denied (see demoReadDeny) so
	// a public demo never discloses topology, the access roster, or unredacted
	// file content (the file browser handler applies its own demo redaction).
	// Writes/actions stay denied.
	if res.demoRead && c.Scope == ScopeServer && c.Verb == VerbRead && !demoReadDeny[capID] {
		return true
	}
	switch c.Scope {
	case ScopePanel:
		return res.panelCaps[capID]
	case ScopeServer:
		return res.ownerSelf || res.serverCaps[capID]
	case ScopeOwner:
		return res.ownerSelf || res.ownerCaps[capID]
	}
	return false
}

// Resolve builds the Resolution. serverID == 0 means no specific server: PANEL
// caps are resolved and the user is treated as owner of their own OWNER realm.
// A non-zero serverID scopes SERVER/OWNER caps to that server's owner realm.
func (r *Resolver) Resolve(id Identity, serverID int) (*Resolution, error) {
	res := &Resolution{
		panelCaps:  map[string]bool{},
		serverCaps: map[string]bool{},
		ownerCaps:  map[string]bool{},
	}
	if id.IsAdmin {
		res.admin = true
		return res, nil
	}
	if id.UserID == "" {
		return res, nil // no identity: deny-by-default
	}

	// PANEL caps: role capabilities plus per-user grant/deny overrides.
	roleID, overrides, err := r.store.GetUserPanelAuthz(id.UserID)
	if err == nil {
		if roleID != nil {
			if role, rerr := r.store.GetPanelRole(*roleID); rerr == nil && role != nil {
				for _, capID := range role.Capabilities {
					res.panelCaps[capID] = true
				}
			}
		}
		applyOverrides(res.panelCaps, overrides)
	}

	if serverID == 0 {
		// Acting on the user's own OWNER realm (their own modpacks/library/etc).
		res.ownerSelf = true
		return res, nil
	}

	srv, serr := r.store.GetServerByID(serverID)
	if serr != nil || srv == nil {
		return res, nil // unknown server: SERVER/OWNER stay empty (deny)
	}
	if r.demoRead != nil && r.demoRead(serverID) {
		res.demoRead = true
	}
	// Owner short-circuit keyed ONLY on the immutable owner UUID. The username is
	// mutable and reusable (a rename frees the old name), so it must never be an
	// authorization key - the UUID is always populated (the owner JOIN drops rows
	// with no valid owner, yielding deny).
	if srv.OwnerID == id.UserID {
		res.ownerSelf = true // server owner short-circuit
		return res, nil
	}

	// Direct per-server grant, else a proxy-inherited grant.
	grant, gerr := r.store.GetServerGrant(serverID, id.UserID)
	if gerr != nil || grant == nil {
		grant = nil
		if srv.ProxyID != nil {
			// Never inherit across owners: the proxy must belong to the SAME owner
			// as the child, else a friend invited on someone else's proxy could
			// reach this owner's server.
			if psrv, perr := r.store.GetServerByID(*srv.ProxyID); perr == nil && psrv != nil && psrv.OwnerID == srv.OwnerID {
				if pg, perr2 := r.store.GetServerGrant(*srv.ProxyID, id.UserID); perr2 == nil && pg != nil && pg.Inherit {
					grant = pg
				}
			}
		}
	}
	if grant != nil {
		r.applyGrant(res, grant)
	}
	// Account-wide grant on this server's owner realm (all servers + owner tools).
	if acct, aerr := r.store.GetAccountGrant(srv.OwnerID, id.UserID); aerr == nil && acct != nil {
		r.applyGrant(res, acct)
	}
	return res, nil
}

// applyGrant folds a grant's server-role caps + overrides into the resolution,
// routing each resolved cap to the SERVER or OWNER set by its catalog scope so
// a mixed server-role (SERVER + OWNER caps) lands in both sets correctly.
func (r *Resolver) applyGrant(res *Resolution, g *store.ServerGrant) {
	caps := map[string]bool{}
	if g.ServerRoleID != nil {
		if role, err := r.store.GetServerRole(*g.ServerRoleID); err == nil && role != nil {
			for _, capID := range role.Capabilities {
				caps[capID] = true
			}
		}
	}
	applyOverrides(caps, g.CapOverrides)
	for capID := range caps {
		c, ok := Get(capID)
		if !ok {
			continue
		}
		switch c.Scope {
		case ScopeOwner:
			res.ownerCaps[capID] = true
		case ScopeServer:
			res.serverCaps[capID] = true
			// A PANEL-scoped cap must never enter the resolution via an
			// owner-scoped grant (server role / invite override): drop it.
		}
	}
}

// HasAnyServerCap reports whether the resolution lets the principal do ANYTHING
// on the server it was built for. It asks HasCap for every SERVER capability in
// the catalog rather than looking at serverCaps, so the admin, owner and demo
// rules are applied exactly once, in HasCap.
//
// This is the question a server LIST has to ask. A grant row existing is not
// the same answer: an account-wide grant may carry only OWNER caps (modpacks,
// backup storage), which reach no server at all, yet it matches every server
// that owner has.
func (res *Resolution) HasAnyServerCap() bool {
	for _, c := range ByScope(ScopeServer) {
		if res.HasCap(c.ID) {
			return true
		}
	}
	return false
}

// applyOverrides adds every Grant cap then removes every Deny cap.
func applyOverrides(m map[string]bool, ov store.CapOverrides) {
	for _, c := range ov.Grant {
		m[c] = true
	}
	for _, c := range ov.Deny {
		delete(m, c)
	}
}

// CapSubset returns the subset of requested capabilities that res actually
// holds. This is the delegation cap: a non-owner assigner can only grant caps
// they themselves hold. Owner/admin resolutions hold everything, so nothing is
// removed.
func CapSubset(res *Resolution, requested []string) []string {
	out := make([]string, 0, len(requested))
	for _, c := range requested {
		if res.HasCap(c) {
			out = append(out, c)
		}
	}
	return out
}
