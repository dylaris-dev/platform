package models

import (
	"strings"
	"time"
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	// Password is the bcrypt hash. json:"-" like the two 2FA fields below it,
	// rather than "password,omitempty": every read path populates it
	// (userSelectCols selects the column), so `omitempty` only helped when a
	// call site remembered to blank it first. Exactly two did, and safety that
	// depends on each future call site remembering is not safety - an offline
	// cracking target is the wrong thing to leave one forgotten assignment away
	// from a response body.
	//
	// It applies on the way IN too, and one wire type does embed this struct:
	// handlers.createUserRequest. It therefore declares its own `password`
	// field to take the plaintext - without that, every admin-created account
	// arrived with an empty password and was refused. Any future request type
	// that embeds User has to do the same.
	Password          string    `json:"-"`
	Email             string    `json:"email"`
	MinecraftUsername string    `json:"minecraftUsername"`
	IsAdmin           bool      `json:"isAdmin"`
	Is2FAEnabled      bool      `json:"is2FAEnabled"`
	TOTPSecret        string    `json:"-"` // never sent to clients
	TOTPBackupCodes   string    `json:"-"` // JSON array of bcrypt-hashed codes
	Permissions       string    `json:"permissions"`
	CreatedAt         time.Time `json:"createdAt"`

	// Region access.
	// AllRegionsAccess=true means "all regions, current and future" — overrides Regions.
	// Regions is populated on demand by handlers that need it (not by default scanUser).
	AllRegionsAccess bool     `json:"allRegionsAccess"`
	Regions          []string `json:"regions,omitempty"`

	// Role + granular capability flags. is_admin (above) is kept
	// in sync with role for backward-compat with handlers that read it.
	Role               string `json:"role"`
	CanDeleteServers   bool   `json:"canDeleteServers"`
	CanChangeResources bool   `json:"canChangeResources"`
	SupportTeam        string `json:"supportTeam,omitempty"`

	// Per-user feature gate for the Modpack Builder. Default TRUE.
	// Admin can flip this to revoke modpack-authoring rights without disabling
	// the feature globally.
	CanCreateModpacks bool `json:"canCreateModpacks"`
	// CanCreateModpacksManual is TRUE once an admin set CanCreateModpacks by
	// hand. The bulk apply behind the platform authoring toggle can then skip
	// this row, so a deliberate per-user decision survives the global switch.
	// Read-only to the panel; it is set as a side effect of the per-user write.
	CanCreateModpacksManual bool `json:"canCreateModpacksManual"`

	// Verification / lifecycle
	EmailVerifiedAt         *time.Time `json:"emailVerifiedAt,omitempty"`
	EmailVerificationToken  string     `json:"-"`
	EmailVerificationSentAt *time.Time `json:"-"`
	PasswordResetToken      string     `json:"-"`
	PasswordResetExpiresAt  *time.Time `json:"-"`
	LastLoginAt             *time.Time `json:"lastLoginAt,omitempty"`
	DeletionStatus          string     `json:"deletionStatus"`
	DeletionWarningSentAt   *time.Time `json:"deletionWarningSentAt,omitempty"`
	DeletionScheduledAt     *time.Time `json:"deletionScheduledAt,omitempty"`

	LastUsernameChange *time.Time `json:"lastUsernameChange,omitempty"`
}

// Region is a geographic deployment region. Single-region setups have one
// row with id='default'; multi-region adds 'eu', 'us-east' etc. The id is
// used as the value of nodes.region / servers.region.
type Region struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	Enabled     bool      `json:"enabled"`
	Color       string    `json:"color,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// AuditEventIdentity is a single append-only audit row for identity-domain
// events (registration, login, role change, deletion, settings change, etc.).
type AuditEventIdentity struct {
	ID           int64                  `json:"id"`
	EventType    string                 `json:"eventType"`
	ActorUserID  *string                `json:"actorUserId,omitempty"`
	TargetUserID *string                `json:"targetUserId,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	IPAddress    string                 `json:"ipAddress,omitempty"`
	UserAgent    string                 `json:"userAgent,omitempty"`
	CreatedAt    time.Time              `json:"createdAt"`
}

// ── Tickets ───────────────────────────────────────────────────────────

// TicketCategory is an admin-curated category. RequiresServer toggles the
// server-picker step in the create form. DefaultAssigneeTeam pre-populates
// the team string on new tickets — drives the cross-team visibility scope.
type TicketCategory struct {
	ID                  int       `json:"id"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	RequiresServer      bool      `json:"requiresServer"`
	DefaultPriority     string    `json:"defaultPriority"`
	DefaultAssigneeTeam string    `json:"defaultAssigneeTeam,omitempty"`
	Color               string    `json:"color,omitempty"`
	Enabled             bool      `json:"enabled"`
	Position            int       `json:"position"`
	CreatedAt           time.Time `json:"createdAt"`
}

// Ticket is the canonical ticket row. ServerUUID/ServerRegion are nullable
// — only set when the category requires a server. AssignedUserID is the
// supporter currently responsible; AssignedTeam carries the
// support_team string and drives the cross-team visibility scope.
type Ticket struct {
	ID           int    `json:"id"`
	Region       string `json:"region"`
	CategoryID   int    `json:"categoryId"`
	CategoryName string `json:"categoryName,omitempty"`
	UserID       string `json:"userId"`
	Username     string `json:"username,omitempty"`
	ServerUUID   string `json:"serverUuid,omitempty"`
	ServerRegion string `json:"serverRegion,omitempty"`
	ServerName   string `json:"serverName,omitempty"`
	// ServerID is the numeric id the panel routes on (/servers/<id>). The row
	// stores the UUID, but every panel link needs the id, so the detail read
	// resolves it alongside the name. Populated by GetTicket only — list views
	// show the name and do not link.
	ServerID int `json:"serverId,omitempty"`
	// What the ticket is about, when it is not a server: "server" | "node" |
	// "route" | "" (nothing specific). SubjectRef names the node or route;
	// for a server the id stays in ServerUUID so there is one place it lives.
	SubjectKind    string     `json:"subjectKind,omitempty"`
	SubjectRef     string     `json:"subjectRef,omitempty"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Priority       string     `json:"priority"`
	AssignedUserID *string    `json:"assignedUserId,omitempty"`
	AssignedName   string     `json:"assignedName,omitempty"`
	AssignedTeam   string     `json:"assignedTeam,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	// MessageCount is populated by list queries for the badge in the inbox UI.
	MessageCount int `json:"messageCount,omitempty"`
	// UnseenInternal is the count of internal notes (only meaningful for
	// support viewers). 0 for the ticket creator.
	UnseenInternal int `json:"unseenInternal,omitempty"`
}

// TicketMessage is one reply on a ticket. IsInternal hides it from the
// creator + watchers — only support+admin see internal notes.
type TicketMessage struct {
	ID         int       `json:"id"`
	TicketID   int       `json:"ticketId"`
	UserID     string    `json:"userId"`
	Username   string    `json:"username,omitempty"`
	UserRole   string    `json:"userRole,omitempty"` // role at time of post (snapshot)
	Body       string    `json:"body"`
	IsInternal bool      `json:"isInternal"`
	CreatedAt  time.Time `json:"createdAt"`
}

// TicketWatcher is a CC participant. CanReply distinguishes read-only
// observers from co-authors. Read-only watchers never see internal notes.
type TicketWatcher struct {
	TicketID int       `json:"ticketId"`
	UserID   string    `json:"userId"`
	Username string    `json:"username,omitempty"`
	CanReply bool      `json:"canReply"`
	AddedAt  time.Time `json:"addedAt"`
	AddedBy  *string   `json:"addedBy,omitempty"`
}

// TicketAuditEvent is an append-only audit row scoped to one ticket.
// Surfaced in the support UI under a "History" tab.
type TicketAuditEvent struct {
	ID          int64                  `json:"id"`
	TicketID    int                    `json:"ticketId"`
	EventType   string                 `json:"eventType"`
	ActorUserID *string                `json:"actorUserId,omitempty"`
	ActorName   string                 `json:"actorName,omitempty"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt   time.Time              `json:"createdAt"`
}

// TicketDeletion is an immutable audit row for an admin-driven ticket
// deletion. Username + category snapshots are stored as plain text so the
// row stays readable after the originating user or category is removed.
// OwnerUserID is nullable because users can be anonymized post-deletion.
type TicketDeletion struct {
	ID            string    `json:"id"`
	TicketID      int       `json:"ticketId"`
	TicketSubject string    `json:"ticketSubject"`
	OwnerUserID   *string   `json:"ownerUserId,omitempty"`
	OwnerUsername string    `json:"ownerUsername"`
	CategoryName  *string   `json:"categoryName,omitempty"`
	DeletedBy     string    `json:"deletedBy"`
	DeletedByName string    `json:"deletedByName"`
	DeletedAt     time.Time `json:"deletedAt"`
	IPAddress     *string   `json:"ipAddress,omitempty"`
	UserAgent     *string   `json:"userAgent,omitempty"`
}

// ── Tickets Polish ───────────────────────────────────────────────────

// TicketAttachment is the metadata row for a file uploaded to a ticket.
// The actual bytes live in the configured StorageProvider under StorageKey.
// MessageID is nullable so attachments can land on the create form before
// any messages exist.
type TicketAttachment struct {
	ID         int       `json:"id"`
	TicketID   int       `json:"ticketId"`
	MessageID  *int      `json:"messageId,omitempty"`
	Filename   string    `json:"filename"`
	Mime       string    `json:"mime"`
	SizeBytes  int64     `json:"sizeBytes"`
	StorageKey string    `json:"-"` // never sent to clients
	UploadedBy *string   `json:"uploadedBy,omitempty"`
	Username   string    `json:"username,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// CannedResponse is an admin-curated snippet support staff insert into
// replies. CategoryID scopes the suggestion to a single ticket category
// (nullable = global). Body supports template variables expanded by the
// frontend at insert time.
type CannedResponse struct {
	ID         int       `json:"id"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	CategoryID *int      `json:"categoryId,omitempty"`
	CreatedBy  *string   `json:"createdBy,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ServerAuditEvent is one row in the per-server audit log.
// Append-only by convention — there's no UPDATE/DELETE in the store layer
// beyond the retention sweep. TargetUserID is set on member-related events
// (invite/remove/permission change) so admins can answer "who was kicked off
// my server" without parsing metadata.
type ServerAuditEvent struct {
	ID           int64                  `json:"id"`
	ServerID     int                    `json:"serverId"`
	Region       string                 `json:"region"`
	EventType    string                 `json:"eventType"`
	ActorUserID  *string                `json:"actorUserId,omitempty"`
	ActorName    string                 `json:"actorName,omitempty"`
	TargetUserID *string                `json:"targetUserId,omitempty"`
	TargetName   string                 `json:"targetName,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
	IPAddress    string                 `json:"ipAddress,omitempty"`
	UserAgent    string                 `json:"userAgent,omitempty"`
	CreatedAt    time.Time              `json:"createdAt"`
}

// ServerAuditState is what GET /servers/{id}/audit/status returns — drives
// the UI toggle that admins use to flip audit_force_on.
type ServerAuditState struct {
	Enabled     bool `json:"enabled"`     // auto-flipped by InviteMember
	ForceOn     bool `json:"forceOn"`     // admin override
	EffectiveOn bool `json:"effectiveOn"` // enabled OR forceOn
	EventCount  int  `json:"eventCount"`  // total rows for this server
}

// Notification is one in-app notification row. Generic enough for any
// producer; tickets are just the first user. Link is the relative URL the
// bell-dropdown anchors the row to.
type Notification struct {
	ID        int64      `json:"id"`
	UserID    string     `json:"userId"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link,omitempty"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type Node struct {
	ID            int        `json:"id"`
	Name          string     `json:"name"`
	Token         string     `json:"token"`
	LinkEnabled   bool       `json:"linkEnabled"`
	LinkInstances int        `json:"linkInstances"`
	LinkSecret    string     `json:"linkSecret"`
	CpusetCpus    string     `json:"cpusetCpus"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastSeenAt    *time.Time `json:"lastSeenAt"`

	// LinkToken is the token of the link the Hub names for this node
	// (nodes.link_token), empty until it has answered. Never serialised: it is
	// that link's tunnel credential. Routes read it through the services
	// effective-token helper, never directly.
	LinkToken string `json:"-"`

	Address string `json:"address"`
	Status  string `json:"status"`
	Tags    string `json:"tags"`
	// DisplayName is an optional, non-unique human label. Defaults to the node's
	// reported hostname at enroll; editable in the Panel. Empty = fall back to Name.
	DisplayName string `json:"displayName,omitempty"`
	Region      string `json:"region"`
	IsLocal     bool   `json:"isLocal"`
	// OwnerID is the BYON tenant who owns this node. nil = platform node
	// (operator-owned). Only meaningful when feature_byon_enabled is on.
	OwnerID    *string  `json:"ownerId,omitempty"`
	PublicIP   string   `json:"publicIp"`
	PrivateIPs []string `json:"privateIps"`

	ServerCount int `json:"serverCount,omitempty"`

	// Placement / overcommit (persisted)
	CPUOvercommitRatio float64 `json:"cpuOvercommitRatio"`
	RAMOvercommitRatio float64 `json:"ramOvercommitRatio"`
	TotalCPU           float64 `json:"totalCpu"`   // physical cores (cached from heartbeat)
	TotalRAMMB         int64   `json:"totalRamMb"` // physical RAM in MB (cached from heartbeat)

	// Live stats from heartbeat (not persisted, -1 = not available)
	CPUUsage  float64 `json:"cpuUsage"`
	RAMFree   int64   `json:"ramFree"`
	RAMTotal  uint64  `json:"ramTotal"`
	LinkCount int     `json:"linkCount"`
	// PortRange is the node's effective MC host-port range ("25600-25699").
	// PortRangeNotice is set only when the node fell back to its default because
	// PORT_RANGE was unset or unparseable - shown so a typo is visible instead
	// of the node quietly binding ports the host firewall does not allow.
	PortRange       string `json:"portRange,omitempty"`
	PortRangeNotice string `json:"portRangeNotice,omitempty"`
	// NetPolicy is the per-server ingress rule state, live from the heartbeat
	// exactly like the fields above. nil is "this node has not said", which an
	// older node and an unreachable one both are.
	//
	// A POINTER, and omitempty, because there are three answers and not two: a
	// plain bool ships `false` for every silent node, and the panel's
	// `=== undefined` guard could then never fire.
	NetPolicy        *bool  `json:"netPolicy,omitempty"`
	NetPolicyServers int    `json:"netPolicyServers,omitempty"`
	NetPolicyNotice  string `json:"netPolicyNotice,omitempty"`
	// SharedStorage is non-empty when this node found one of its storage paths
	// mounted into another node as well. That topology cannot work - node
	// identity itself lives in the first storage path - and it destroys a server
	// on the next migration while reporting success.
	SharedStorage []SharedStorageConflict `json:"sharedStorage,omitempty"`

	// Unusable is a derived (not persisted) flag set at API-response time:
	// true when this node currently has no usable routing path (e.g. an
	// external node while platform routing is ip_port). UnusableReason gives
	// the panel a short machine-readable cause. Empty/false = node is usable.
	Unusable       bool   `json:"unusable,omitempty"`
	UnusableReason string `json:"unusableReason,omitempty"`

	// Configured is a legacy marker: an admin adopted this node through the
	// Configure dialog, which no longer exists. Tags and region follow the
	// node's environment again regardless of it; all it still does is stop the
	// heartbeat renaming a row that was renamed by hand back then. Nothing sets
	// it any more.
	Configured bool `json:"configured"`
	// NeedsConfiguration is a derived (not persisted) flag set at API-response
	// time: true when the node reports no region, i.e. it was started without
	// NODE_REGION. The fix is on the node, not here.
	NeedsConfiguration bool `json:"needsConfiguration,omitempty"`
}

// SharedStorageConflict is one storage path a node found mounted into another
// node as well. Mirrors what the node publishes in its heartbeat.
//
// Kind is "peer" (another node's beacon sits on our disk) or the strictly worse
// "identity" (another PROCESS is writing our own beacon, so .node_secret and
// .node_id have already collided and the two nodes overwrite each other).
// Shared node storage is not supported - it is detected and surfaced, never
// worked around.
type SharedStorageConflict struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	PeerNode string `json:"peerNode,omitempty"`
	PeerHost string `json:"peerHost,omitempty"`
}

// IsExternal reports whether this node is an external/home node (tag "external").
// External nodes force gateway+beam locally, so SFTP + direct ports are unusable.
// NodeKind is whose machine this is. Three answers, and the difference is
// ownership rather than capability: a BYON machine belongs to a customer who
// has root on it, an external one is the operator's own hardware outside the
// swarm, and everything else is the platform's.
type NodeKind string

const (
	NodeKindPlatform NodeKind = "platform"
	NodeKindExternal NodeKind = "external"
	NodeKindBYON     NodeKind = "byon"
)

// Kind classifies the node. Ownership is asked FIRST, deliberately: a tenant
// machine that also carries the external tag is still the customer's, and
// getting that order wrong files a customer's box under the operator's own
// hardware with nothing failing anywhere.
//
// An owner pointer to the empty string is treated as no owner. It is a data
// anomaly either way - nothing can own a node under an empty id, and the
// ownership check elsewhere already refuses to match one - so the two readings
// differ only in which heading such a row appears under, and "unowned" is the
// safer of the two. The panel's lib/nodeKind mirrors this exactly.
func (n *Node) Kind() NodeKind {
	if n.OwnerID != nil && *n.OwnerID != "" {
		return NodeKindBYON
	}
	if n.IsExternal() {
		return NodeKindExternal
	}
	return NodeKindPlatform
}

func (n *Node) IsExternal() bool {
	for _, t := range strings.Split(n.Tags, ",") {
		if strings.TrimSpace(t) == "external" {
			return true
		}
	}
	return false
}

type Server struct {
	ID          int    `json:"id"`
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	NodeID      int    `json:"nodeId"`
	NodeName    string `json:"node"`
	NodeAddress string `json:"nodeAddress"`
	// Node reachability, joined from nodes for the honest connectivity display.
	// NodeStatus is the node's own online/offline - distinct from Status, which is
	// the server's last node-pushed status and freezes when the node goes away.
	NodeStatus     string     `json:"nodeStatus"`
	NodeLastSeenAt *time.Time `json:"nodeLastSeenAt"`
	// NodeKind is whose machine this server runs on, derived from the node row
	// rather than carried on it. The derived answer travels and the inputs do
	// not: nodes.owner_id would tell every reader WHICH account owns the
	// hardware, which is a different fact than the one a server list needs.
	NodeKind  NodeKind `json:"nodeKind"`
	OwnerID   string   `json:"ownerId"`
	OwnerName string   `json:"owner"`
	GameImage string   `json:"image"`
	Port      int      `json:"port"`
	Memory    int      `json:"memory"`
	CPULimit  float64  `json:"cpuLimit"`
	// CPUPinningMode: 'shared' (default), 'auto' or 'manual'. Cpuset is the
	// effective core list (e.g. "0-3,8"), empty when shared/unpinned.
	CPUPinningMode   string `json:"cpuPinningMode"`
	Cpuset           string `json:"cpuset"`
	StartCommand     string `json:"startCommand"`
	Status           string `json:"status"`
	DesiredState     string `json:"desiredState"`
	IsFixed          bool   `json:"isFixed"`
	ActiveSubServer  string `json:"activeSubServer"`
	ExtraJvmFlags    string `json:"extraJvmFlags"`
	InstallerType    string `json:"installerType"`
	MinecraftVersion string `json:"minecraftVersion"`
	BuildNumber      string `json:"buildNumber"`
	DiskLimit        int64  `json:"diskLimit"`
	HostPort         int    `json:"hostPort"`
	ContainerPort    int    `json:"containerPort"`
	ServerType       string `json:"serverType"`
	ProxyID          *int   `json:"proxyId"`
	AutoMove         bool   `json:"autoMove"`
	Region           string `json:"region"`
	// RCON config. RconEnabled controls whether the server writes
	// enable-rcon=true to server.properties on next launch and whether the
	// panel surfaces RCON-driven UIs (Players tab, /rcon endpoint). RconPort
	// 0 = MC default 25575; RconPassword is stored separately (encrypted) and
	// never serialized to JSON.
	RconEnabled  bool            `json:"rconEnabled"`
	RconPort     int             `json:"rconPort"`
	RconPassword string          `json:"-"`
	CreatedAt    time.Time       `json:"createdAt"`
	Role         string          `json:"role,omitempty"`
	Permissions  *TabPermissions `json:"permissions,omitempty"`
	// IsDemo is set at response time (not a DB column) when this server is on the
	// admin's demo list. Non-owner viewers get read-only access; the panel uses
	// this to render a read-only badge and suppress write affordances.
	IsDemo bool `json:"isDemo,omitempty"`
	// InstallStalled is set at response time (not a DB column) when this server
	// reads "installing" but nothing is working on it, because the node holding
	// the job is offline. The status stays "installing" because it is still true;
	// this is the missing explanation, not a state change. See
	// handlers.annotateStalledInstalls.
	InstallStalled     bool   `json:"installStalled,omitempty"`
	InstallStallReason string `json:"installStallReason,omitempty"`
}

// TabPermissions defines per-tab access rights for invited users
type TabPermissions struct {
	Console  bool `json:"console"`
	Files    bool `json:"files"`
	Config   bool `json:"config"`
	Setup    bool `json:"setup"`
	Overview bool `json:"overview"`
	Power    bool `json:"power"`
	// Players is newer than the rest and is deliberately absent from the legacy
	// invite blob's stored shape: an old row decodes it false, and the resolved
	// players.read is what turns it on (see handlers.tabPermissionCaps). A
	// legacy invite carrying Power maps to players.read in MapLegacyInviteCaps,
	// so those keep the tab they had.
	Players bool `json:"players"`
	Members bool `json:"members"`
	Network bool `json:"network"`
	Backups bool `json:"backups"`
	Inherit bool `json:"inherit"`
}

// ServerInvite represents an invitation for a user to access a server
type ServerInvite struct {
	ID          int            `json:"id"`
	ServerID    int            `json:"serverId"`
	UserID      string         `json:"userId"`
	Username    string         `json:"username"`
	Email       string         `json:"email"`
	Permissions TabPermissions `json:"permissions"`
	InvitedBy   string         `json:"invitedBy"`
	InviterName string         `json:"inviterName"`
	CreatedAt   time.Time      `json:"createdAt"`
}

// ServerStatRow represents a single stats data point stored in PostgreSQL
type ServerStatRow struct {
	Time       time.Time `json:"time"`
	ServerUUID string    `json:"serverUuid"`
	CPU        float64   `json:"cpu"`
	CPULimit   float64   `json:"cpuLimit"`
	MemUsed    int64     `json:"memUsed"`
	MemLimit   int64     `json:"memLimit"`
	Players    int       `json:"players"`
	MaxPlayers int       `json:"maxPlayers"`
}

// GatewayBandwidthRow is one downsampled bandwidth sample for a gateway
// component (edge/warp/beam), stored in gateway_bandwidth_stats.
type GatewayBandwidthRow struct {
	Time      time.Time `json:"time"`
	Component string    `json:"component"`
	ID        string    `json:"id"`
	Host      string    `json:"host"`
	Region    string    `json:"region"`
	RxBps     uint64    `json:"rxBps"`
	TxBps     uint64    `json:"txBps"`
	CapMbit   int       `json:"capMbit"`
}

type Module struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Icon       string `json:"icon"`
	URL        string `json:"url"`
	IsEnabled  bool   `json:"isEnabled"`
	IsSystem   bool   `json:"isSystem"`
	Position   int    `json:"position"`
	AccessRole string `json:"accessRole"` // "all" | "admin"
}

// --- Gateway Route Limits (managed by Core, not Hub) ---

// GatewayRouteLimit caps how many protected addresses ON OUR DOMAINS one scope
// may hold. Scopes resolve most-specific-first: "user:<id>", then "user_default",
// then "global".
//
// MaxRoutes follows the platform convention (services.Limits): NULL is no cap at
// all, 0 is a real "none", n is the cap. A missing ROW is different again - it
// means this scope says nothing, so the next one down is asked.
//
// The column used to be NOT NULL DEFAULT 0 with 0 meaning "disabled", which was
// the one place in the platform where 0 did not mean unlimited. A store that
// granted zero addresses had that zero rewritten into "no override", which fell
// through to a scope nobody had set - and that is unlimited. Making the absence
// its own value is what stops a computed zero from being mistaken for one.
// TrafficLimit is what one scope says about one (region, kind): how much is
// included, and how much may be bought on top.
//
// Both are pointers because both follow the platform-wide limit convention -
// nil is no limit, 0 is none, n is the cap (CLAUDE.md, "Limits"). Read them
// through services.Exceeds / services.AtOrOver rather than by hand.
type TrafficLimit struct {
	ID            int    `json:"id"`
	Scope         string `json:"scope"`
	Region        string `json:"region"`
	Kind          string `json:"kind"`
	IncludedGB    *int64 `json:"includedGb"`
	MaxPurchaseGB *int64 `json:"maxPurchaseGb"`
}

type GatewayRouteLimit struct {
	ID        int    `json:"id"`
	Scope     string `json:"scope"`
	MaxRoutes *int   `json:"maxRoutes"`
}

// MailTemplate is an operator's override of one outgoing mail. It exists only
// once the mail has been edited: no row means the built-in wording, so "reset to
// default" is a delete rather than a copy of the original text back in.
type MailTemplate struct {
	Key       string    `json:"key"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NodeJoinAttempt is a connection Core REFUSED, kept so an operator can see it
// and decide whether to let that machine back in.
//
// Read the trust boundary off the field names: PeerIP is observed from the
// socket, everything else is what the caller SAID about itself. The panel shows
// both and labels them, because an operator recognising their own machine is the
// whole purpose - but only the address can be relied on when deciding.
type NodeJoinAttempt struct {
	NodeToken string `json:"nodeToken"`
	// The node row this identity belongs to, resolved for display. Only known
	// identities are ever recorded.
	NodeName    string `json:"nodeName"`
	DisplayName string `json:"displayName"`

	PeerIP string `json:"peerIp"`

	ReportedPublicIP   string `json:"reportedPublicIp"`
	ReportedPrivateIPs string `json:"reportedPrivateIps"`
	Hostname           string `json:"hostname"`
	CPUCores           int    `json:"cpuCores"`
	CPUModel           string `json:"cpuModel"`
	MemoryBytes        int64  `json:"memoryBytes"`
	ReleaseVersion     string `json:"releaseVersion"`

	Reason      string    `json:"reason"`
	Attempts    int       `json:"attempts"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`

	// ApprovedUntil is set while an admission is armed. It is deliberately short
	// and tied to ApprovedFromIP: the identity in this row is self-claimed, so an
	// approval that never expired would admit whoever knocks with it next.
	ApprovedUntil  *time.Time `json:"approvedUntil,omitempty"`
	ApprovedFromIP string     `json:"approvedFromIp,omitempty"`
	ApprovedBy     string     `json:"approvedBy,omitempty"`
}
