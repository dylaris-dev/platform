package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"dylaris-core/store"
	"dylaris-pkg/errlog"

	"github.com/redis/go-redis/v9"
)

const hubQueueKey = "dylaris:hub:queue"

// --- Redis-side types for reading gateway state ---

// GatewayEdgeInfo represents an Edge as stored in Redis (edge:registry:{id}).
// Stats are not part of the registry payload — they're published separately to
// the dylaris:edge:{id}:stats stream by the Edge service every few seconds and
// merged in by GetEdgesFromRedis.
type GatewayEdgeInfo struct {
	EdgeID      string `json:"edge_id"`
	Name        string `json:"name"`
	IP          string `json:"ip"`
	PrivateIP   string `json:"private_ip"`
	ServicePort string `json:"service_port"`
	SplicePort  string `json:"splice_port"`
	Status      string `json:"status"`
	// Region + Wildcard are advertised by regional edges for the DNS updater.
	// Region groups edges (eu/us); Wildcard is the A-record name the reconciler
	// points at this region's edge IPs (e.g. "*.eu.dylaris.com"). Empty on edges
	// not configured for multi-region DNS.
	Region   string         `json:"region,omitempty"`
	Wildcard string         `json:"wildcard,omitempty"`
	Stats    *EdgeLiveStats `json:"stats,omitempty"`
	// SpliceVersion is the RUNNING splice sidecar's version on this edge;
	// SpliceVersionLatest is the LATEST available version (baked into the edge's
	// own rolling :latest image). Both come straight from the edge heartbeat. The
	// panel compares them per region to surface a pending splice bump. Empty when a
	// pre-versioning splice/edge is deployed.
	SpliceVersion       string `json:"splice_version,omitempty"`
	SpliceVersionLatest string `json:"splice_version_latest,omitempty"`
	// SpliceImageRunning is the image the splice CONTAINER runs;
	// SpliceImageAvailable is what the edge's SPLICE_IMAGE reference resolves
	// to. The version pair above cannot answer "is the running splice the
	// pinned one": a tag deleted from the registry frees the name for a later
	// build, so two different images report the same version string - which is
	// what splice-0.13.0 did on both production edges while the panel read them
	// as current. Empty means the edge could not tell, and must never be read
	// as agreement.
	SpliceImageRunning   string `json:"splice_image_running,omitempty"`
	SpliceImageAvailable string `json:"splice_image_available,omitempty"`
}

// EdgeLiveStats mirrors the EdgeMetrics payload published by the Edge service
// to dylaris:edge:{id}:stats. Field names match the JSON the Edge produces.
type EdgeLiveStats struct {
	CPU                 float64 `json:"cpu"`
	RAMUsed             uint64  `json:"ram_used"`
	RAMTotal            uint64  `json:"ram_total"`
	RAMPercent          float64 `json:"ram_pct"`
	RxSpeed             uint64  `json:"rx_speed"`
	TxSpeed             uint64  `json:"tx_speed"`
	ActiveTunnels       int     `json:"active_tunnels"`
	ActiveTokens        int     `json:"active_tokens"`
	ActiveMCStreams     int64   `json:"active_mc_streams"`
	XDPEnabled          bool    `json:"xdp_enabled"`
	XDPPassed           uint64  `json:"xdp_passed"`
	XDPDroppedBlocked   uint64  `json:"xdp_dropped_blocked"`
	XDPDroppedRateLimit uint64  `json:"xdp_dropped_ratelimit"`
	XDPBlockedIPs       int     `json:"xdp_blocked_ips"`
}

// GatewayLinkStatus represents a link's online state as readable from Redis.
type GatewayLinkStatus struct {
	Token  string `json:"token"`
	Online bool   `json:"online"`
}

// GatewayRoute represents a route as stored in Redis (route:{domain}).
type GatewayRoute struct {
	Domain     string `json:"domain"`
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`
	TunnelID   string `json:"tunnel_id"` // link token (the customer's route-only Link, or a managed node's Link)
	ServerUUID string `json:"server_uuid"`
	// CoreOwned marks a route Core publishes DIRECTLY to Redis (not via the Hub
	// DB): the tenant route-only kits. It still rides the normal Link tunnel path
	// (TunnelID set) — the flag only tells the Hub's zombie sweep to leave it
	// alone, since the Hub doesn't own its lifecycle.
	CoreOwned bool `json:"core_owned,omitempty"`
	// OwnerID (user UUID) is published for Core-owned routes so the panel can list
	// and authorize them per owner. Empty for server-bound (managed) routes.
	OwnerID string `json:"owner_id,omitempty"`
}

// LinkFingerprint names a link without handing over its token.
//
// A link's identity IS its token (the Redis key is link:<token>), so a listing
// has to identify one somehow. A SHA-256 prefix is stable across polls, tells
// two links apart, and cannot be presented to an edge as a tunnel claim.
func LinkFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// PublicRoute is a route as it may be shown to a CLIENT. It is GatewayRoute
// minus the tunnel token.
//
// That token is a credential, not a description. The edge admits a tunnel that
// presents it (IsValidTokenStrict against link:<token>) and then round-robins
// player streams across every session registered under it - so a second holder
// does not merely observe, it receives about half of the connections meant for
// the first and answers them itself.
//
// For a MANAGED server the token is derived from the NODE, so one route's
// tunnel_id is the credential for every server on that node. It was reachable
// through GET /api/servers/{id}/routes, which needs only network.read on one
// server - a capability an ordinary tenant holds on their own. Nothing has ever
// read the field on the client side; it was serialised because the whole struct
// was.
type PublicRoute struct {
	Domain     string `json:"domain"`
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`
	ServerUUID string `json:"server_uuid"`
	CoreOwned  bool   `json:"core_owned,omitempty"`
	OwnerID    string `json:"owner_id,omitempty"`
}

// Public strips the tunnel token off one route.
func (r GatewayRoute) Public() PublicRoute {
	return PublicRoute{
		Domain:     r.Domain,
		TargetIP:   r.TargetIP,
		TargetPort: r.TargetPort,
		ServerUUID: r.ServerUUID,
		CoreOwned:  r.CoreOwned,
		OwnerID:    r.OwnerID,
	}
}

// PublicRoutes strips the tunnel token off a list, preserving order. Returns a
// non-nil empty slice so a handler encodes [] rather than null.
func PublicRoutes(routes []GatewayRoute) []PublicRoute {
	out := make([]PublicRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Public())
	}
	return out
}

// hubQueueMessage is the payload pushed to dylaris:hub:queue.
type hubQueueMessage struct {
	Action       string  `json:"action"`
	Domain       string  `json:"domain,omitempty"`
	TargetIP     string  `json:"target_ip,omitempty"`
	TargetPort   int     `json:"target_port,omitempty"`
	LinkToken    string  `json:"link_token,omitempty"`
	ServerUUID   string  `json:"server_uuid,omitempty"`
	ServerID     *uint   `json:"server_id,omitempty"`
	OwnerID      *string `json:"owner_id,omitempty"`
	NewLinkToken string  `json:"new_link_token,omitempty"`
}

// coreOwnedRouteTTL is 0 (persistent). Core-owned (route-only) routes are
// published to Redis by Core directly (not via the hub DB), so they have no
// link-bound 24h refresh; Core owns their create/delete and the hub's zombie
// sweep skips them.
const coreOwnedRouteTTL = 0

// ErrRouteDomainTaken is returned when route:{domain} already belongs to
// someone else. route:{domain} is a single global namespace with two writers -
// the hub, from its own routes table, and Core, for route-only entries - and
// only the hub half has a uniqueness constraint (a unique index on domain).
// Nothing enforced the other half, so a plain SET handed a domain from one
// tenant to the next. CheckDomainAvailability has always reported the collision
// to the panel; it was only ever a hint, never a gate.
var ErrRouteDomainTaken = errors.New("domain is already routed")

// coreOwnedRouteHolder reads route:{domain} and reports whether it is a
// route-only (Core-owned) entry, plus the user UUID that owns it. ok is false
// when the key is absent, unreadable or not JSON - callers treat that as
// "someone else holds it" and refuse, because a domain we cannot prove is free
// is not free.
func (g *RedisGateway) coreOwnedRouteHolder(ctx context.Context, domain string) (ownerID string, coreOwned bool, ok bool) {
	val, err := g.redis.Get(ctx, "route:"+domain).Result()
	if err != nil {
		return "", false, false
	}
	var r GatewayRoute
	if json.Unmarshal([]byte(val), &r) != nil {
		return "", false, false
	}
	return r.OwnerID, r.CoreOwned, true
}

// --- GatewayProvider interface ---

// GatewayProvider handles route lifecycle operations (write path only).
// Reads are done directly from Redis using the helper functions below.
type GatewayProvider interface {
	CreateServerRoute(serverID uint, ownerID string, domain string, port int) error
	// CreateRouteViaLink publishes a route-only entry that rides the tenant's own
	// Link tunnel: the edge opens a stream on linkToken and the customer's Link
	// dials targetHost:port on its LOCAL network. No managed node, no exposed
	// origin, splice + rolling updates preserved.
	CreateRouteViaLink(ownerID string, domain string, linkToken string, targetHost string, port int) error
	DeleteCoreOwnedRoute(domain string) error
	DeleteRoute(domain string) error
	MigrateServerRoutes(serverID uint, newNodeID uint) error
	// LinkToken derives the deterministic Link tunnel token for a link identity
	// (a warp key's node_id). Core holds the cluster secret; tenants receive only
	// the derived token via their kit, never the secret itself.
	LinkToken(nodeID string) string
	// DiscoveryProof derives the Link discovery-heartbeat proof for a link
	// identity, so a secret-free BYON Link can prove its identity without ever
	// holding CLUSTER_SECRET.
	DiscoveryProof(nodeID string) string
}

// --- RedisGateway (active in gateway / both routing modes) ---

type RedisGateway struct {
	redis         *redis.Client
	store         store.Store
	clusterSecret string
}

func NewRedisGateway(r *redis.Client, s store.Store, secret string) *RedisGateway {
	return &RedisGateway{redis: r, store: s, clusterSecret: secret}
}

func (g *RedisGateway) CreateServerRoute(serverID uint, ownerID string, domain string, port int) error {
	// 1. Resolve server
	server, err := g.store.GetServerByID(int(serverID))
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}

	// 2. Resolve node → the token of the link that serves it
	node, err := g.store.GetNodeByID(server.NodeID)
	if err != nil {
		return fmt.Errorf("node not found: %w", err)
	}
	linkToken := effectiveLinkToken(node, g.clusterSecret)

	// 3. Port enable checks (limit counts skipped — Hub enforces uniqueness in its DB)
	if port == 25565 {
		if val, _ := g.store.GetSetting("gateway_port_mc_enabled"); val == "false" {
			return fmt.Errorf("minecraft port (25565) routing is disabled")
		}
	}

	// 4. Refuse to queue a route that would land on a tenant's route-only entry.
	// The hub enforces uniqueness only within its OWN routes table, and a
	// route-only entry has no row there - so the create would succeed, and the
	// hub's next sync would SET route:{domain} straight over the tenant's.
	// Managed-vs-managed needs no check here: that collision hits the hub's
	// unique index, which keeps the first route. Adding one would misfire
	// anyway, because a deleted managed route's Redis key survives until the
	// leader's next sweep.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, coreOwned, ok := g.coreOwnedRouteHolder(ctx, domain); ok && coreOwned {
		return ErrRouteDomainTaken
	}

	// 5. Push to queue
	sID := uint(serverID)
	oID := ownerID
	msg := hubQueueMessage{
		Action:     "create_route",
		Domain:     domain,
		TargetIP:   fmt.Sprintf("mc_%s", server.UUID),
		TargetPort: port,
		LinkToken:  linkToken,
		ServerUUID: server.UUID,
		ServerID:   &sID,
		OwnerID:    &oID,
	}

	return g.pushToQueue(msg)
}

// CreateRouteViaLink registers a route-only entry by publishing it DIRECTLY to
// Redis (route:{domain} + sys:index:routes), bypassing the hub's queue/DB. The
// route carries the tenant's Link token as tunnel_id, so the edge uses the normal
// tunnel path: it opens a stream on that tunnel and the customer's Link dials the
// LOCAL target. The hub does not own this route (Core direct-published it), it
// only learns to skip it in the zombie sweep via the core_owned flag.
//
// It also records the route durably (core_link_routes). Redis stays the live
// routing table and the atomic claim, but it is no longer the only copy: see
// RepublishCoreOwnedRoutes.
func (g *RedisGateway) CreateRouteViaLink(ownerID string, domain string, linkToken string, targetHost string, port int) error {
	if port == 25565 {
		if val, _ := g.store.GetSetting("gateway_port_mc_enabled"); val == "false" {
			return fmt.Errorf("minecraft port (25565) routing is disabled")
		}
	}
	if linkToken == "" {
		return fmt.Errorf("link token is required")
	}
	// Durable ownership is asked FIRST, and it outranks the Redis claim.
	// SETNX alone was a sufficient arbiter only while Redis held the only copy:
	// with a stored row behind it, an emptied Redis would let a second tenant
	// claim a domain the first one still owns on paper, and the republisher
	// would then fight the live entry every tick. The row answers even when the
	// cache is gone, which is precisely when the question gets asked.
	if stored, err := g.storedRoute(domain); err != nil {
		return err
	} else if stored != nil && stored.OwnerID != ownerID {
		return ErrRouteDomainTaken
	}
	data, err := json.Marshal(map[string]interface{}{
		"domain":      domain,
		"target_ip":   targetHost,
		"target_port": port,
		"tunnel_id":   linkToken,
		"server_uuid": "",
		"core_owned":  true,
		"owner_id":    ownerID,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// SETNX, not SET: one domain, one owner. Two tenants can post the same
	// domain at the same moment, so the claim has to be atomic - a read-then-
	// write here would let both through and hand the loser's players to the
	// winner's Link.
	claimed, err := g.redis.SetNX(ctx, "route:"+domain, data, coreOwnedRouteTTL).Result()
	if err != nil {
		return err
	}
	if !claimed {
		// Taken. The only permitted overwrite is this same tenant rewriting
		// their OWN route-only entry, which is how they change its target host
		// or port. Anything else - another tenant's entry, or a managed
		// server's route (no core_owned flag, written by the hub) - is refused.
		holder, coreOwned, ok := g.coreOwnedRouteHolder(ctx, domain)
		if !ok || !coreOwned || holder != ownerID {
			return ErrRouteDomainTaken
		}
		if err := g.redis.Set(ctx, "route:"+domain, data, coreOwnedRouteTTL).Err(); err != nil {
			return err
		}
	}
	if err := g.store.UpsertCoreLinkRoute(store.CoreLinkRoute{
		Domain: domain, OwnerID: ownerID, LinkToken: linkToken,
		TargetHost: targetHost, TargetPort: port,
	}); err != nil {
		// Give the claim back. A live Redis entry with no row behind it is
		// worse than no route at all: the panel would list it, the tenant was
		// told the create failed, and every path that removes a route now works
		// from the rows - so nothing could ever take it away again.
		if claimed {
			g.redis.Del(ctx, "route:"+domain)
		}
		return err
	}
	return g.redis.SAdd(ctx, "sys:index:routes", domain).Err()
}

// storedRoute reads the durable record for one domain, or nil when there is
// none. A read FAILURE is an error rather than a nil: "no row" and "could not
// ask" lead to opposite decisions at every call site here.
func (g *RedisGateway) storedRoute(domain string) (*store.CoreLinkRoute, error) {
	rows, err := g.store.ListCoreLinkRoutes()
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Domain == domain {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// DeleteCoreOwnedRoute removes a route-only entry: the durable row first, then
// the Redis entry.
//
// That order is deliberate. The row is what RepublishCoreOwnedRoutes writes
// back, so dropping the cache entry first would leave a window in which the
// republisher restores the route the caller is deleting - and if the process
// dies between the two steps, the route comes back. Deleting the row first
// makes the worst case a stale cache entry that the next delete, or the tenant,
// can still clear.
func (g *RedisGateway) DeleteCoreOwnedRoute(domain string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.store.DeleteCoreLinkRoute(domain); err != nil {
		return err
	}
	if err := g.redis.Del(ctx, "route:"+domain).Err(); err != nil {
		return err
	}
	return g.redis.SRem(ctx, "sys:index:routes", domain).Err()
}

// DeleteRoute removes a route whoever owns it.
//
// It used to ONLY push to the hub's queue, and that is wrong for half the routes
// this platform has. A route-only entry is Core's: Core direct-published it to
// Redis and recorded it in core_link_routes, and the hub has no row for it - so
// the queue message deleted nothing, RepublishCoreOwnedRoutes wrote the stored
// row straight back on its next tick, and the address kept routing. Deleting
// from the admin Routes screen looked like it worked and did nothing, because
// RPush succeeding is not the hub having acted.
//
// So both halves run, in this order:
//
//   - The durable row first, then the Redis entry. Same reason
//     DeleteCoreOwnedRoute gives: the row is what the republisher writes back, so
//     clearing the cache first leaves a window for it to restore what is being
//     deleted. Row first makes the worst case a stale cache entry.
//   - Then the queue message, for a route the HUB owns. Harmless for one it does
//     not: DeleteRouteByDomain deletes zero rows without erroring and re-syncs.
//
// A caller that knows the route is Core's should still use DeleteCoreOwnedRoute -
// it skips a queue round trip that cannot apply. This one is for the paths that
// cannot tell, which is every admin-facing delete.
func (g *RedisGateway) DeleteRoute(domain string) error {
	stored, err := g.storedRoute(domain)
	if err != nil {
		return err
	}
	if stored != nil {
		if err := g.DeleteCoreOwnedRoute(domain); err != nil {
			return err
		}
	}
	return g.pushToQueue(hubQueueMessage{
		Action: "delete_route",
		Domain: domain,
	})
}

func (g *RedisGateway) MigrateServerRoutes(serverID uint, newNodeID uint) error {
	server, err := g.store.GetServerByID(int(serverID))
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}

	node, err := g.store.GetNodeByID(int(newNodeID))
	if err != nil {
		return fmt.Errorf("node not found: %w", err)
	}
	newToken := effectiveLinkToken(node, g.clusterSecret)

	return g.pushToQueue(hubQueueMessage{
		Action:       "migrate_routes",
		ServerUUID:   server.UUID,
		NewLinkToken: newToken,
	})
}

// LinkToken derives the Link tunnel token for a link identity (warp key node_id).
func (g *RedisGateway) LinkToken(nodeID string) string {
	return DeriveLinkToken(nodeID, g.clusterSecret)
}

// DiscoveryProof derives the Link discovery-heartbeat proof for a link identity.
func (g *RedisGateway) DiscoveryProof(nodeID string) string {
	return DeriveDiscoveryProof(nodeID, g.clusterSecret)
}

func (g *RedisGateway) pushToQueue(msg hubQueueMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return g.redis.RPush(ctx, hubQueueKey, string(data)).Err()
}

// DeriveLinkToken computes SHA256(nodeID + clusterSecret) — same as Link and Hub binaries.
func DeriveLinkToken(nodeID, clusterSecret string) string {
	h := sha256.New()
	h.Write([]byte(nodeID + clusterSecret))
	return hex.EncodeToString(h.Sum(nil))
}

// DeriveDiscoveryProof computes SHA256("discovery:"+nodeID+":"+clusterSecret) -
// byte-identical to the Link + Hub binaries' deriveDiscoveryProof. Delivered to a
// BYON Link so it never needs the raw CLUSTER_SECRET for its discovery heartbeat.
func DeriveDiscoveryProof(nodeID, clusterSecret string) string {
	h := sha256.New()
	h.Write([]byte("discovery:" + nodeID + ":" + clusterSecret))
	return hex.EncodeToString(h.Sum(nil))
}

// --- Redis read helpers ---

// GetEdgesFromRedis reads all edges from Redis (edge:registry:{id} keys) and
// merges the latest stats snapshot from each edge's stats stream.
func GetEdgesFromRedis(ctx context.Context, rdb *redis.Client) []GatewayEdgeInfo {
	var edges []GatewayEdgeInfo
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "edge:registry:*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			val, err := rdb.Get(ctx, key).Result()
			if err != nil {
				continue
			}
			var e GatewayEdgeInfo
			if json.Unmarshal([]byte(val), &e) == nil {
				if e.Status == "" {
					e.Status = "online"
				}
				e.Stats = readLatestEdgeStats(ctx, rdb, e.EdgeID)
				edges = append(edges, e)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return edges
}

// readLatestEdgeStats fetches the newest entry from the per-edge stats stream
// written by the Edge service's metrics collector. Returns nil if the stream
// is empty, missing, or unreadable — never blocks the overview response.
func readLatestEdgeStats(ctx context.Context, rdb *redis.Client, edgeID string) *EdgeLiveStats {
	if edgeID == "" {
		return nil
	}
	streamKey := "dylaris:edge:" + edgeID + ":stats"
	msgs, err := rdb.XRevRangeN(ctx, streamKey, "+", "-", 1).Result()
	if err != nil || len(msgs) == 0 {
		return nil
	}
	raw, ok := msgs[0].Values["data"].(string)
	if !ok || raw == "" {
		return nil
	}
	var s EdgeLiveStats
	if json.Unmarshal([]byte(raw), &s) != nil {
		return nil
	}
	return &s
}

// GetLinksFromRedis reads all known link tokens and their online status from Redis.
func GetLinksFromRedis(ctx context.Context, rdb *redis.Client) []GatewayLinkStatus {
	// Tokens with active keep-alive — one self-expiring key per live link
	// (online_link:<token>, 15s TTL). A dead link's key expires on its own,
	// unlike the old shared set whose single TTL any live link refreshed.
	onlineMap := make(map[string]bool)
	var oCursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, oCursor, "online_link:*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			onlineMap[key[len("online_link:"):]] = true
		}
		oCursor = next
		if oCursor == 0 {
			break
		}
	}

	// All known tokens from link:{token} keys
	var links []GatewayLinkStatus
	var cursor uint64
	seen := make(map[string]bool)
	for {
		keys, next, err := rdb.Scan(ctx, cursor, "link:*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			token := key[5:] // strip "link:"
			if seen[token] {
				continue
			}
			seen[token] = true

			// Presence only. This used to also sum sys:stats:tunnels:<token> into
			// an active-tunnel count and OR it into Online, but nothing in either
			// repository has ever written that key - so the count was always zero
			// and the OR always a no-op.
			links = append(links, GatewayLinkStatus{
				Token:  token,
				Online: onlineMap[token],
			})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return links
}

// GetRoutesFromRedis reads all routes from Redis (sys:index:routes + route:{domain}).
func GetRoutesFromRedis(ctx context.Context, rdb *redis.Client) []GatewayRoute {
	domains, err := rdb.SMembers(ctx, "sys:index:routes").Result()
	if err != nil {
		return nil
	}
	routes := make([]GatewayRoute, 0, len(domains))
	for _, domain := range domains {
		val, err := rdb.Get(ctx, "route:"+domain).Result()
		if err != nil {
			continue
		}
		var r GatewayRoute
		if json.Unmarshal([]byte(val), &r) == nil {
			r.Domain = domain
			routes = append(routes, r)
		}
	}
	return routes
}

// CountRoutesFromRedis returns the total number of routes in Redis.
func CountRoutesFromRedis(ctx context.Context, rdb *redis.Client) int64 {
	count, _ := rdb.SCard(ctx, "sys:index:routes").Result()
	return count
}

// GetServiceErrorsFromRedis reads error log entries from Redis Streams.
func GetServiceErrorsFromRedis(rdb *redis.Client, service string, count int64) []errlog.Entry {
	streams, err := errlog.ScanErrorStreams(rdb, service)
	if err != nil {
		return nil
	}
	perStream := count
	if len(streams) > 1 {
		perStream = count / int64(len(streams))
		if perStream < 10 {
			perStream = 10
		}
	}
	var all []errlog.Entry
	for _, stream := range streams {
		entries, err := errlog.ReadEntries(rdb, stream, perStream)
		if err != nil {
			continue
		}
		all = append(all, entries...)
	}
	return all
}

// ErrorStreamServices are the service names whose Redis error streams the panel
// reads. A name here must match the FIRST argument every producer passes to
// errlog.New, because that argument is what forms the stream key
// (dylaris:errors:<service>:<instance>) - a scan for a name nobody writes finds
// nothing and reports it as "no errors".
//
// "edge" replaced "gate", and the gap it left is why this list is now a named
// constant with a test behind it. The service was renamed gate -> edge and its
// producer moved with it (edge/cmd/main.go passes "edge"), but this list did
// not: Core kept scanning dylaris:errors:gate:* while every edge wrote to
// dylaris:errors:edge:*. The ACL granted the new name, so the writes succeeded
// and the reads simply matched nothing - the panel showed an empty edge section,
// which looks exactly like a healthy edge. The component carrying all player
// traffic had no error reporting at all, and nothing anywhere failed to say so.
//
// "hub" belongs here: the Hub logs a dropped queue message to its own error
// stream, and leaving it out of this list is why a malformed create_route
// could fail silently for months while the panel still reported success.
// "beam" for the same reason: a relay that refuses to register over a
// region/host conflict writes the explanation here and nowhere else.
// "node" is the platform side rather than the gateway: it is the one component
// that can still reach Redis while its control channel to Core is broken, so it
// is the only thing able to report WHY a node looks offline.
// "core" is the platform side too, and the one that took longest to arrive:
// Core runs every periodic job - dunning, the backup scheduler, the retention
// sweeps, the ACL reconciler, the route republisher - and reported their
// failures to its own stdout and nowhere else. A container's stdout dies with
// the container, so a job that had been failing every night left no trace once
// Core was redeployed, which is the first thing anyone does when something
// looks wrong. See services/errsink.go for the producer side.
var ErrorStreamServices = errlog.Services

// GetAllServiceErrorsFromRedis reads errors for every service in
// ErrorStreamServices.
func GetAllServiceErrorsFromRedis(rdb *redis.Client, count int64) map[string][]errlog.Entry {
	result := make(map[string][]errlog.Entry)
	for _, svc := range ErrorStreamServices {
		entries := GetServiceErrorsFromRedis(rdb, svc, count)
		if len(entries) > 0 {
			result[svc] = entries
		}
	}
	return result
}
