// Package authz is the single source of truth for the capability-based
// permission system: the code-defined capability catalog, the EffectiveCaps
// resolver, and the RequireCap enforcement middleware. Phase 1 (foundation)
// ships the catalog, resolver, middleware and route-coverage harness; the
// ~400 routes are annotated and the old checks removed in phase 2.
package authz

// Scope classifies where a capability is enforced.
type Scope string

const (
	// ScopePanel is a platform-wide staff/operator capability, checked against
	// the user's panel role + per-user overrides.
	ScopePanel Scope = "panel"
	// ScopeServer is a per-server capability, checked against the invite/grant
	// for the {id}/{uuid} in the request path.
	ScopeServer Scope = "server"
	// ScopeOwner is an owner's cross-server tools/artifacts (modpacks, library,
	// the owner's custom server-roles, the owner's API keys).
	ScopeOwner Scope = "owner"
)

// Verb is the action portion of a capability ID. Read/write/delete are used
// wherever CRUD applies; explicit action verbs are used otherwise.
type Verb string

const (
	VerbRead    Verb = "read"
	VerbWrite   Verb = "write"
	VerbDelete  Verb = "delete"
	VerbCreate  Verb = "create"
	VerbRestore Verb = "restore"
	VerbStart   Verb = "start"
	VerbStop    Verb = "stop"
	VerbRestart Verb = "restart"
	VerbKill    Verb = "kill"
	VerbExec    Verb = "exec"
	VerbAccess  Verb = "access"
	VerbUse     Verb = "use"
	VerbManage  Verb = "manage"
	VerbSend    Verb = "send"
)

// Capability is one entry in the catalog. ID is the stored/enforced unit and
// is globally unique across all scopes (the coverage test + API-key vocabulary
// depend on uniqueness). Where a resource name would collide across scopes the
// ID is disambiguated (e.g. SERVER network.* vs PANEL topology.*, OWNER roles.*
// vs PANEL panelroles.*, PANEL settings.* vs SERVER server.settings.write).
type Capability struct {
	ID       string
	Label    string
	Category string
	Scope    Scope
	Verb     Verb
}

// catalog is the ordered single source of truth. Order is preserved by All(),
// ByScope() and Grouped() so the frontend renders a stable list. This is the
// representative structured set for phase 1; phase 2 confirms every one of the
// ~400 routes maps to exactly one of these IDs.
var catalog = []Capability{
	// --- SERVER scope ---
	{ID: "overview.read", Label: "View overview", Category: "Overview", Scope: ScopeServer, Verb: VerbRead},

	{ID: "console.read", Label: "Read console", Category: "Console", Scope: ScopeServer, Verb: VerbRead},
	{ID: "console.send", Label: "Send console command", Category: "Console", Scope: ScopeServer, Verb: VerbSend},

	{ID: "power.start", Label: "Start server", Category: "Power", Scope: ScopeServer, Verb: VerbStart},
	{ID: "power.stop", Label: "Stop server", Category: "Power", Scope: ScopeServer, Verb: VerbStop},
	{ID: "power.restart", Label: "Restart server", Category: "Power", Scope: ScopeServer, Verb: VerbRestart},
	{ID: "power.kill", Label: "Kill server", Category: "Power", Scope: ScopeServer, Verb: VerbKill},

	{ID: "rcon.exec", Label: "Run RCON command", Category: "RCON", Scope: ScopeServer, Verb: VerbExec},

	{ID: "players.read", Label: "View players", Category: "Players", Scope: ScopeServer, Verb: VerbRead},
	{ID: "players.manage", Label: "Manage players", Category: "Players", Scope: ScopeServer, Verb: VerbManage},

	{ID: "files.read", Label: "Read files", Category: "Files", Scope: ScopeServer, Verb: VerbRead},
	{ID: "files.write", Label: "Write files", Category: "Files", Scope: ScopeServer, Verb: VerbWrite},
	{ID: "files.delete", Label: "Delete files", Category: "Files", Scope: ScopeServer, Verb: VerbDelete},

	{ID: "sftp.access", Label: "Use SFTP", Category: "SFTP", Scope: ScopeServer, Verb: VerbAccess},

	{ID: "config.read", Label: "Read configuration", Category: "Config", Scope: ScopeServer, Verb: VerbRead},
	{ID: "config.write", Label: "Write configuration", Category: "Config", Scope: ScopeServer, Verb: VerbWrite},

	{ID: "mods.read", Label: "View mods", Category: "Mods", Scope: ScopeServer, Verb: VerbRead},
	{ID: "mods.write", Label: "Install/update mods", Category: "Mods", Scope: ScopeServer, Verb: VerbWrite},
	{ID: "mods.delete", Label: "Remove mods", Category: "Mods", Scope: ScopeServer, Verb: VerbDelete},

	{ID: "backups.read", Label: "View backups", Category: "Backups", Scope: ScopeServer, Verb: VerbRead},
	{ID: "backups.create", Label: "Create backup", Category: "Backups", Scope: ScopeServer, Verb: VerbCreate},
	{ID: "backups.delete", Label: "Delete backup", Category: "Backups", Scope: ScopeServer, Verb: VerbDelete},
	{ID: "backups.restore", Label: "Restore backup", Category: "Backups", Scope: ScopeServer, Verb: VerbRestore},

	// A tenant's OWN object storage for backups, across all their servers, which
	// is why it is owner-scoped rather than per server. Separate from
	// backups.* because it is a different thing to trust somebody with:
	// backups.create runs a backup to a target the platform chose, this decides
	// where the bytes GO and holds an S3 credential.
	//
	// Not granted by any preset. Connecting arbitrary S3 endpoints makes Core
	// talk to hosts an operator did not pick, so it is a deliberate grant rather
	// than something that arrives with a role.
	{ID: "backupstorage.read", Label: "View own backup storage", Category: "Backups", Scope: ScopeOwner, Verb: VerbRead},
	{ID: "backupstorage.write", Label: "Connect own backup storage", Category: "Backups", Scope: ScopeOwner, Verb: VerbWrite},
	{ID: "backupstorage.delete", Label: "Remove own backup storage", Category: "Backups", Scope: ScopeOwner, Verb: VerbDelete},

	{ID: "network.read", Label: "View network", Category: "Network", Scope: ScopeServer, Verb: VerbRead},
	{ID: "network.write", Label: "Edit network", Category: "Network", Scope: ScopeServer, Verb: VerbWrite},

	{ID: "tabs.read", Label: "View custom tabs", Category: "Tabs", Scope: ScopeServer, Verb: VerbRead},
	{ID: "tabs.write", Label: "Manage custom tabs", Category: "Tabs", Scope: ScopeServer, Verb: VerbWrite},

	{ID: "schedule.read", Label: "View scheduled tasks", Category: "Schedule", Scope: ScopeServer, Verb: VerbRead},
	{ID: "schedule.write", Label: "Manage scheduled tasks", Category: "Schedule", Scope: ScopeServer, Verb: VerbWrite},
	{ID: "schedule.delete", Label: "Delete scheduled tasks", Category: "Schedule", Scope: ScopeServer, Verb: VerbDelete},

	{ID: "stats.read", Label: "View stats", Category: "Stats", Scope: ScopeServer, Verb: VerbRead},

	{ID: "spark.use", Label: "Use Spark profiler", Category: "Spark", Scope: ScopeServer, Verb: VerbUse},

	{ID: "members.read", Label: "View members", Category: "Members", Scope: ScopeServer, Verb: VerbRead},
	{ID: "members.write", Label: "Invite/edit members", Category: "Members", Scope: ScopeServer, Verb: VerbWrite},
	{ID: "members.delete", Label: "Remove members", Category: "Members", Scope: ScopeServer, Verb: VerbDelete},

	{ID: "server.settings.write", Label: "Edit server settings", Category: "Server", Scope: ScopeServer, Verb: VerbWrite},
	{ID: "server.delete", Label: "Delete server", Category: "Server", Scope: ScopeServer, Verb: VerbDelete},

	// The per-server audit trail is the OWNER's accountability record over the
	// people they invited: it carries every actor's IP address and user agent.
	// It therefore gets its own cap instead of riding on overview.read, which
	// every invite grants - that made the record readable by exactly the people
	// it records. Deliberately NOT in any preset (presets.go): "Full access"
	// hands a friend the server, not the log of what that friend did and where
	// from. An owner who wants to delegate it does so explicitly in Admin-roles
	// mode. Owner and panel admin hold it via the resolver short-circuits.
	{ID: "server.audit.read", Label: "View server audit log", Category: "Audit", Scope: ScopeServer, Verb: VerbRead},

	// --- OWNER scope ---
	{ID: "modpack.read", Label: "View modpacks", Category: "Modpacks", Scope: ScopeOwner, Verb: VerbRead},
	{ID: "modpack.write", Label: "Edit modpacks", Category: "Modpacks", Scope: ScopeOwner, Verb: VerbWrite},
	{ID: "modpack.delete", Label: "Delete modpacks", Category: "Modpacks", Scope: ScopeOwner, Verb: VerbDelete},

	{ID: "library.read", Label: "View library", Category: "Library", Scope: ScopeOwner, Verb: VerbRead},
	{ID: "library.write", Label: "Edit library", Category: "Library", Scope: ScopeOwner, Verb: VerbWrite},
	{ID: "library.delete", Label: "Delete library items", Category: "Library", Scope: ScopeOwner, Verb: VerbDelete},

	{ID: "roles.read", Label: "View server-roles", Category: "Server-roles", Scope: ScopeOwner, Verb: VerbRead},
	{ID: "roles.write", Label: "Edit server-roles", Category: "Server-roles", Scope: ScopeOwner, Verb: VerbWrite},
	{ID: "roles.delete", Label: "Delete server-roles", Category: "Server-roles", Scope: ScopeOwner, Verb: VerbDelete},

	// Account-level metered usage (traffic + backup storage), i.e. the numbers a
	// bill is computed from. It exists as its own capability because the only
	// route that consumes it is API-key-authed: an owner hands a key to an
	// integrator for one server, and without a gate that key would also read the
	// whole account's billing figures, which the key's server allowlist cannot
	// narrow. Making it mintable makes that an explicit choice.
	//
	// A session never needs it: /api/me/usage answers the CALLER's own usage and
	// is capability-exempt for exactly that reason. So granting this in a
	// server-role changes nothing today - it is a key capability that happens to
	// live in the shared catalog, which is where key capabilities live.
	{ID: "usage.read", Label: "View account usage", Category: "Usage", Scope: ScopeOwner, Verb: VerbRead},

	{ID: "apikeys.read", Label: "View API keys", Category: "API keys", Scope: ScopeOwner, Verb: VerbRead},
	{ID: "apikeys.write", Label: "Create API keys", Category: "API keys", Scope: ScopeOwner, Verb: VerbWrite},
	{ID: "apikeys.delete", Label: "Revoke API keys", Category: "API keys", Scope: ScopeOwner, Verb: VerbDelete},

	// --- PANEL scope ---
	{ID: "users.read", Label: "View users", Category: "Users", Scope: ScopePanel, Verb: VerbRead},
	{ID: "users.write", Label: "Edit users", Category: "Users", Scope: ScopePanel, Verb: VerbWrite},
	{ID: "users.delete", Label: "Delete users", Category: "Users", Scope: ScopePanel, Verb: VerbDelete},

	{ID: "panelroles.read", Label: "View panel roles", Category: "Panel roles", Scope: ScopePanel, Verb: VerbRead},
	{ID: "panelroles.write", Label: "Edit panel roles", Category: "Panel roles", Scope: ScopePanel, Verb: VerbWrite},
	{ID: "panelroles.delete", Label: "Delete panel roles", Category: "Panel roles", Scope: ScopePanel, Verb: VerbDelete},

	{ID: "nodes.read", Label: "View nodes", Category: "Nodes", Scope: ScopePanel, Verb: VerbRead},
	{ID: "nodes.write", Label: "Edit nodes", Category: "Nodes", Scope: ScopePanel, Verb: VerbWrite},
	{ID: "nodes.delete", Label: "Delete nodes", Category: "Nodes", Scope: ScopePanel, Verb: VerbDelete},

	{ID: "topology.read", Label: "View global topology", Category: "Topology", Scope: ScopePanel, Verb: VerbRead},
	{ID: "topology.write", Label: "Edit global topology", Category: "Topology", Scope: ScopePanel, Verb: VerbWrite},

	{ID: "regions.read", Label: "View regions", Category: "Regions", Scope: ScopePanel, Verb: VerbRead},
	{ID: "regions.write", Label: "Edit regions", Category: "Regions", Scope: ScopePanel, Verb: VerbWrite},
	{ID: "regions.delete", Label: "Delete regions", Category: "Regions", Scope: ScopePanel, Verb: VerbDelete},

	{ID: "tickets.read", Label: "View tickets", Category: "Tickets", Scope: ScopePanel, Verb: VerbRead},
	{ID: "tickets.write", Label: "Edit tickets", Category: "Tickets", Scope: ScopePanel, Verb: VerbWrite},
	{ID: "tickets.delete", Label: "Delete tickets", Category: "Tickets", Scope: ScopePanel, Verb: VerbDelete},

	{ID: "settings.read", Label: "View platform settings", Category: "Settings", Scope: ScopePanel, Verb: VerbRead},
	// settings.write carries the same dial-a-host power the comment above
	// backupstorage.write describes, and that is easy to miss because its label
	// says "settings": it gates the database-migration connection test, the
	// metrics-database test and the storage-migration target, each of which
	// makes Core connect to a host the CALLER names. There is no field on a
	// capability for saying that to whoever builds a role, so it is said here.
	//
	// It is more than that: it is full admin in all but name, like
	// panelroles.write. It copies the whole database to a host the caller names
	// (POST /api/admin/db/migration), and it sets the platform-backup passphrase
	// and downloads or restores those backups - every account, hash and secret
	// on the platform. Grant it to no one you would not make an admin.
	//
	// Today only 'admin' holds it - the seeded 'support' role excludes it by
	// name. A custom role that includes it inherits all of this too.
	{ID: "settings.write", Label: "Edit platform settings", Category: "Settings", Scope: ScopePanel, Verb: VerbWrite},

	{ID: "plans.read", Label: "View plans", Category: "Plans", Scope: ScopePanel, Verb: VerbRead},
	{ID: "plans.write", Label: "Edit plans", Category: "Plans", Scope: ScopePanel, Verb: VerbWrite},

	{ID: "servers.read", Label: "View all servers", Category: "Servers (oversight)", Scope: ScopePanel, Verb: VerbRead},
	{ID: "servers.write", Label: "Edit any server", Category: "Servers (oversight)", Scope: ScopePanel, Verb: VerbWrite},
	// "servers.delete" (Delete any server) used to sit here, and the whole
	// "Modpacks (staff)" category below it. Both were declared and enforced
	// NOWHERE - no DELETE under /api/admin/servers, and modpacks gated only by
	// the owner-scoped modpack.* caps - so the role editor offered permissions
	// that conferred nothing. Removed rather than listed as exempt; if staff
	// oversight for either is wanted, the endpoint comes first and the
	// capability with it. TestEveryCapabilityReachesAChokepoint now refuses a
	// catalog entry with no chokepoint.

	{ID: "audit.read", Label: "View audit log", Category: "Audit", Scope: ScopePanel, Verb: VerbRead},
}

var byID = func() map[string]Capability {
	m := make(map[string]Capability, len(catalog))
	for _, c := range catalog {
		m[c.ID] = c
	}
	return m
}()

// Get returns the capability for id, and ok=false when it is not in the
// catalog. An unknown capability is deny-by-default at every call site.
func Get(id string) (Capability, bool) {
	c, ok := byID[id]
	return c, ok
}

// Has reports whether id is a known capability.
func Has(id string) bool {
	_, ok := byID[id]
	return ok
}

// All returns a copy of the ordered catalog.
func All() []Capability {
	out := make([]Capability, len(catalog))
	copy(out, catalog)
	return out
}

// ByScope returns the catalog filtered to one scope, order preserved.
func ByScope(s Scope) []Capability {
	var out []Capability
	for _, c := range catalog {
		if c.Scope == s {
			out = append(out, c)
		}
	}
	return out
}

// --- Grouped UI payload (GET /api/authz/catalog) ---

type CatalogCapability struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Verb  string `json:"verb"`
}

type CatalogCategory struct {
	Category     string              `json:"category"`
	Capabilities []CatalogCapability `json:"capabilities"`
}

type CatalogScope struct {
	Scope      string            `json:"scope"`
	Categories []CatalogCategory `json:"categories"`
}

// Grouped returns the catalog grouped by scope then category, for the panel
// role editor and simple/advanced UI. Scope order is fixed [server, owner,
// panel]; categories and capabilities keep registry order so the payload is
// stable across requests.
func Grouped() []CatalogScope {
	scopeOrder := []Scope{ScopeServer, ScopeOwner, ScopePanel}
	out := make([]CatalogScope, 0, len(scopeOrder))
	for _, sc := range scopeOrder {
		cs := CatalogScope{Scope: string(sc)}
		catIndex := map[string]int{} // category -> index in cs.Categories
		for _, c := range catalog {
			if c.Scope != sc {
				continue
			}
			idx, ok := catIndex[c.Category]
			if !ok {
				cs.Categories = append(cs.Categories, CatalogCategory{Category: c.Category})
				idx = len(cs.Categories) - 1
				catIndex[c.Category] = idx
			}
			cs.Categories[idx].Capabilities = append(cs.Categories[idx].Capabilities, CatalogCapability{
				ID: c.ID, Label: c.Label, Verb: string(c.Verb),
			})
		}
		out = append(out, cs)
	}
	return out
}
