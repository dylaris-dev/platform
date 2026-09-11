package store

import "time"

// WarpAPIKey is an enrollment credential with a connection policy.
type WarpAPIKey struct {
	ID        int
	Name      string
	KeyHash   string
	Policy    string // "fixed" | "general"
	MaxConns  int
	OnNewConn string // "kill_old" | "block"
	FixedWGIP string // "" = auto-allocate
	NodeID    string
	Region    string // "" = auto-assign at enroll; else pin enrolls to this region
	OwnerID   string // "" = admin-minted (platform); else the tenant the key/route-only kit belongs to
	// BoundNodeID is the nodes.id a BYON node key belongs to; 0 = not bound yet.
	BoundNodeID int
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// WarpPeer is one enrolled client: pubkey -> allocated WG IP, pinned to a region.
type WarpPeer struct {
	ID             int
	APIKeyID       int
	Pubkey         string
	WGIP           string
	Region         string
	AssignedLeader string // F3 rebalancer "home" leaderID; "" = unpinned (falls back to freest-first)
	CreatedAt      time.Time
}

// WarpRegion owns one WG identity: a subnet and (implicitly) a key derived from
// CLUSTER_SECRET+region. Its leaders are interchangeable endpoints for it.
type WarpRegion struct {
	Region    string
	Subnet    string // e.g. "10.99.1.0/24"
	Enabled   bool
	CreatedAt time.Time
}

// WarpLeader is one redundant endpoint serving a region. All leaders of a region
// share the region's key + subnet + peer set; they differ only by Endpoint.
type WarpLeader struct {
	LeaderID  string
	Region    string
	Endpoint  string // "host:port" reachable by clients
	Enabled   bool
	CreatedAt time.Time
}
