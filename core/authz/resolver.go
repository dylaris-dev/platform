package authz

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"dylaris-core/models"
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
	store       Store
	demoRead    func(serverUUID string) bool // optional; StoreEnabled-gated by the caller
	foreignNode func(srv *models.Server, userID string) bool
	writeAudit  WriteAuditFunc // optional; see write_audit.go
}

func NewResolver(st Store) *Resolver {
	return &Resolver{store: st}
}

// SetDemoRead installs an optional predicate that reports whether a server (by
// UUID - the resolver already holds the row) is an admin-flagged public
// read-only showcase. nil-safe: unset means no demo access.
func (r *Resolver) SetDemoRead(fn func(serverUUID string) bool) { r.demoRead = fn }

// SetForeignNode installs the predicate that reports whether the node a server
// runs on is somebody else's hardware for this user, while node ownership is in
// force. It gets the server row the resolver loaded, which carries the node's
// owner from the same query. An admin gets
// no server rights there beyond what that somebody granted them. It is a hook
// rather than a store call because the answer also depends on the BYON flag,
// which lives in services, and services imports authz.
func (r *Resolver) SetForeignNode(fn func(srv *models.Server, userID string) bool) {
	r.foreignNode = fn
}

// Resolution is the materialized decision context for one (identity, scope).
// It is deny-by-default: a capability is granted only when a short-circuit or
// an explicit resolved cap covers it. HasCap answers individual checks.
type Resolution struct {
	admin      bool            // panel admin: holds every capability
	adminPanel bool            // panel admin on a server on somebody else's node: every PANEL cap, server rights only by grant
	ownerSelf  bool            // owns the realm in scope (own server, or serverID==0 self-realm)
	demoRead   bool            // server is an admin-flagged demo showcase: grant SERVER read caps to any authed principal
	panelCaps  map[string]bool // PANEL caps from panel role + per-user overrides
	// panelLoad fills panelCaps on the first PANEL check. A server route
	// checks a SERVER cap, and reading the panel role for it cost one or two
	// statements whose answer HasCap never looked at.
	panelLoad  func(map[string]bool)
	panelOnce  sync.Once
	serverCaps map[string]bool // SERVER caps from direct/proxy/account grant
	ownerCaps  map[string]bool // OWNER caps from an account grant on this realm
	// server is the row the decision was made on (nil when none was read).
	// RequireCap hands it to the handler for this request only (ServerByID).
	server *models.Server
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
	// backups.read lists the runs and hands out a download of each archive:
	// the whole world, unredacted - server.properties, plugin configs and
	// their credentials - which is exactly what files.read is kept from.
	"backups.read": true,
	// domains.read answers with the node's direct address, the same endpoint
	// disclosure network.read is kept from.
	"domains.read": true,
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
	if res.adminPanel && c.Scope == ScopePanel {
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
		if res.panelLoad != nil {
			res.panelOnce.Do(func() { res.panelLoad(res.panelCaps) })
		}
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
		if serverID == 0 || r.foreignNode == nil {
			return res, nil
		}
		// An operator runs the platform, not their customers' machines. Server ids
		// are sequential, so this has to be decided here, where every server route
		// arrives, and not by hiding the server from a list. A server that does not
		// exist keeps the short-circuit, it grants nothing that exists; one that
		// cannot be READ does not, or a database fault would open whatever it is.
		srv, serr := r.store.GetServerByID(serverID)
		if errors.Is(serr, sql.ErrNoRows) {
			return res, nil
		}
		if serr == nil {
			res.server = srv
		}
		if serr == nil && srv != nil && !r.foreignNode(srv, id.UserID) {
			return res, nil
		}
		res.admin = false
		res.adminPanel = true
		// Fall through: the owner's invite still works, with its own scope.
	}
	if id.UserID == "" {
		return res, nil // no identity: deny-by-default
	}

	r.setPanelLoad(res, id.UserID)

	if serverID == 0 {
		// Acting on the user's own OWNER realm (their own modpacks, roles, keys).
		res.ownerSelf = true
		return res, nil
	}

	srv := res.server
	if srv == nil || srv.ID != serverID {
		var serr error
		srv, serr = r.store.GetServerByID(serverID)
		if serr != nil || srv == nil {
			return res, nil // unknown server: SERVER/OWNER stay empty (deny)
		}
		res.server = srv
	}
	r.resolveOnServer(res, id, srv, r.storeGrantReads(id.UserID))
	return res, nil
}

// setPanelLoad arranges the PANEL caps: role capabilities plus per-user
// grant/deny overrides, read on the first PANEL check. A failed read leaves
// the set empty (deny), as it did when this ran eagerly.
func (r *Resolver) setPanelLoad(res *Resolution, userID string) {
	res.panelLoad = func(caps map[string]bool) {
		roleID, overrides, err := r.store.GetUserPanelAuthz(userID)
		if err != nil {
			return
		}
		if roleID != nil {
			if role, rerr := r.store.GetPanelRole(*roleID); rerr == nil && role != nil {
				for _, capID := range role.Capabilities {
					caps[capID] = true
				}
			}
		}
		applyOverrides(caps, overrides)
	}
}

// grantReads are the reads the server half of a resolution makes. Resolve
// answers them from the store per call; ResolveListed from one prefetch.
type grantReads struct {
	// pair: the caller's direct grant on serverID and account-wide grant in
	// owner's realm. nil = none (a failed read is none).
	pair func(serverID int, owner string) (direct, account *store.ServerGrant)
	// direct: the caller's direct grant on a proxy, for inheritance.
	direct func(serverID int) *store.ServerGrant
	// server: a proxy's row. nil = unreadable.
	server func(id int) *models.Server
}

func (r *Resolver) storeGrantReads(userID string) grantReads {
	return grantReads{
		// One statement where the store can (grantPairStore).
		pair: func(serverID int, owner string) (direct, account *store.ServerGrant) {
			if gp, ok := r.store.(grantPairStore); ok {
				direct, account, _ = gp.GetServerAndAccountGrants(serverID, owner, userID)
				return direct, account
			}
			if g, err := r.store.GetServerGrant(serverID, userID); err == nil {
				direct = g
			}
			if a, err := r.store.GetAccountGrant(owner, userID); err == nil {
				account = a
			}
			return direct, account
		},
		direct: func(serverID int) *store.ServerGrant {
			if g, err := r.store.GetServerGrant(serverID, userID); err == nil {
				return g
			}
			return nil
		},
		server: func(id int) *models.Server {
			if s, err := r.store.GetServerByID(id); err == nil {
				return s
			}
			return nil
		},
	}
}

// resolveOnServer is the SERVER/OWNER half of a resolution for a known server
// row: demo read, the owner short-circuit, then the grants.
func (r *Resolver) resolveOnServer(res *Resolution, id Identity, srv *models.Server, g grantReads) {
	if r.demoRead != nil && r.demoRead(srv.UUID) {
		res.demoRead = true
	}
	// Owner short-circuit keyed ONLY on the immutable owner UUID. The username is
	// mutable and reusable (a rename frees the old name), so it must never be an
	// authorization key - the UUID is always populated (the owner JOIN drops rows
	// with no valid owner, yielding deny).
	if srv.OwnerID == id.UserID {
		res.ownerSelf = true // server owner short-circuit
		return
	}

	// Direct per-server grant and the account-wide grant on this realm.
	grant, acct := g.pair(srv.ID, srv.OwnerID)
	// A grant belongs to the realm of the owner who gave it. An admin owner
	// change moves the server and leaves its rows behind, so without this the
	// previous owner's friends kept their access, and the previous owner could
	// still widen it by editing the server role those rows point at.
	if grant != nil && grant.OwnerUserID != "" && grant.OwnerUserID != srv.OwnerID {
		grant = nil
	}
	// No direct grant: a proxy-inherited one.
	if grant == nil {
		if srv.ProxyID != nil {
			// Never inherit across owners: the proxy must belong to the SAME owner
			// as the child, else a friend invited on someone else's proxy could
			// reach this owner's server.
			if psrv := g.server(*srv.ProxyID); psrv != nil && psrv.OwnerID == srv.OwnerID {
				if pg := g.direct(*srv.ProxyID); pg != nil && pg.Inherit &&
					(pg.OwnerUserID == "" || pg.OwnerUserID == srv.OwnerID) {
					grant = pg
				}
			}
		}
	}
	if grant != nil {
		r.applyGrant(res, grant, srv.OwnerID)
	}
	// Account-wide grant on this server's owner realm (all servers + owner tools).
	if acct != nil {
		r.applyGrant(res, acct, srv.OwnerID)
	}
}

// grantListStore reads every grant a user holds, roles joined, in one
// statement (*store.PostgresStore).
type grantListStore interface {
	ListGrantsForUser(userID string) ([]store.ServerGrant, error)
}

// ResolveListed is Resolve(id, s.ID) for every server a list query just
// returned, for a NON-admin identity, with the caller's grants read once for
// all of them instead of per server. The rows themselves come from that list
// query, in this request. The decision code is the same resolveOnServer;
// only where the grants and a proxy's row come from differs. A store that
// cannot list grants is resolved one server at a time.
func (r *Resolver) ResolveListed(id Identity, servers []models.Server) map[int]*Resolution {
	out := make(map[int]*Resolution, len(servers))
	if len(servers) == 0 {
		return out
	}
	gl, ok := r.store.(grantListStore)
	if !ok || id.IsAdmin || id.UserID == "" {
		for i := range servers {
			if res, err := r.Resolve(id, servers[i].ID); err == nil {
				out[servers[i].ID] = res
			}
		}
		return out
	}
	grants, err := gl.ListGrantsForUser(id.UserID)
	if err != nil {
		return out // no resolution: every row is dropped, as a failed Resolve was
	}
	direct := map[int]*store.ServerGrant{}
	account := map[string]*store.ServerGrant{}
	for i := range grants {
		if g := &grants[i]; g.ServerID != nil {
			direct[*g.ServerID] = g
		} else {
			account[g.OwnerUserID] = g
		}
	}
	listed := make(map[int]*models.Server, len(servers))
	for i := range servers {
		listed[servers[i].ID] = &servers[i]
	}
	storeReads := r.storeGrantReads(id.UserID)
	reads := grantReads{
		pair: func(serverID int, owner string) (*store.ServerGrant, *store.ServerGrant) {
			return direct[serverID], account[owner]
		},
		direct: func(serverID int) *store.ServerGrant { return direct[serverID] },
		server: func(sid int) *models.Server {
			if s := listed[sid]; s != nil {
				return s
			}
			return storeReads.server(sid)
		},
	}
	for i := range servers {
		res := &Resolution{panelCaps: map[string]bool{}, serverCaps: map[string]bool{}, ownerCaps: map[string]bool{}}
		r.setPanelLoad(res, id.UserID)
		srv := servers[i]
		res.server = &srv
		r.resolveOnServer(res, id, &srv, reads)
		out[srv.ID] = res
	}
	return out
}

// applyGrant folds a grant's server-role caps + overrides into the resolution,
// routing each resolved cap to the SERVER or OWNER set by its catalog scope so
// a mixed server-role (SERVER + OWNER caps) lands in both sets correctly.
// realmOwner is the server's owner. A role is used only if it belongs to that
// realm: a grant row edited after an admin owner change takes the new owner's
// realm but keeps its role id, which would otherwise hand the member whatever
// the previous owner puts into that role.
func (r *Resolver) applyGrant(res *Resolution, g *store.ServerGrant, realmOwner string) {
	caps := map[string]bool{}
	if g.ServerRoleID != nil {
		role := g.Role // joined by GetServerAndAccountGrants
		if role == nil {
			role, _ = r.store.GetServerRole(*g.ServerRoleID)
		}
		if role != nil && (role.OwnerUserID == "" || role.OwnerUserID == realmOwner) {
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
