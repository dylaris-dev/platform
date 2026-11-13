package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
	"dylaris-pkg/validate"
)

// Outbox ops queued by RemoteGateway and replayed by the outbox drain.
const (
	OutboxOpDelete = "delete"
	OutboxOpMove   = "move"
	OutboxOpProxy  = "proxy"
	// OutboxOpRevokeKit revokes a link kit; its domain column carries the LINK
	// ID, not a domain. It is not a route: the delete-supersedes rule and the
	// reconcile never see it.
	OutboxOpRevokeKit = "revoke_kit"
)

// OutboxMove is the payload of a "move": put the route on another node's kit.
type OutboxMove struct {
	LinkID     string `json:"linkId"`
	NodeID     uint   `json:"nodeId"`
	TargetHost string `json:"targetHost"`
	Port       int    `json:"port"`
}

// OutboxProxy is the payload of a "proxy": the route's real-IP switch.
type OutboxProxy struct {
	Enabled bool `json:"enabled"`
}

// ErrGatewayTargetName refuses a server whose id cannot form the mc-<uuid>
// hostname the provider routes to; guessing another name would route nowhere.
var ErrGatewayTargetName = errors.New("server id cannot be used as a gateway target")

// ErrGatewayChangePending refuses a create while an older queued write for the
// same domain is open: replayed after the create, an old delete would remove
// the fresh route.
var ErrGatewayChangePending = errors.New("a previous change to this address is still being applied, try again in a moment")

// ErrGatewayDomainTaken refuses an address another server of this platform
// holds. Every server here shares one provider account, so the provider would
// read the second create as the holder editing its own route.
var ErrGatewayDomainTaken = errors.New("this address is already in use")

const (
	remoteCallTimeout = 15 * time.Second
	remoteKitsTTL     = 30 * time.Second
	remoteRegionsTTL  = 60 * time.Second
	// The provider's usage figures change once a minute.
	remoteUsageTTL = 60 * time.Second
	// A failed provider read is answered from the cache for this long, so a
	// provider that does not answer costs one timeout per window, not one per
	// panel poll.
	remoteErrTTL = 10 * time.Second
	// Check-domain answers, keyed by domain: the picker asks per keystroke
	// pause, and every ask is a read on the platform's one provider key.
	remoteCheckTTL = 10 * time.Second
	remoteCheckMax = 1000
	// warp_api_keys.name on the provider is VARCHAR(128) and the mint does not
	// trim it, so a longer node name would fail the provider's insert.
	remoteKitNameMax = 128
)

// RemoteGateway implements GatewayProvider against the managed gateway API of
// our Core. Creates are synchronous so the caller sees the provider's refusal;
// every other write goes through the gateway outbox.
type RemoteGateway struct {
	st  store.Store
	api func() *remotegw.Client
	now func() time.Time

	kits    remoteCache[[]remotegw.Kit]
	regions remoteCache[remotegw.Regions]
	usage   remoteCache[remotegw.Usage]

	checkMu sync.Mutex
	checks  map[string]checkAnswer
}

type checkAnswer struct {
	available bool
	reason    string
	exp       time.Time
}

// remoteCache holds one provider read: the answer for its ttl, a failure for
// remoteErrTTL, and at most one call in flight. The call runs on its own
// context, so one impatient caller cannot cache a cancellation for everyone;
// each caller waits for it with its own context. No lock is held across the
// call.
type remoteCache[T any] struct {
	mu     sync.Mutex
	val    T
	err    error
	exp    time.Time
	gen    int // bumped by drop, so a read started before it is not stored
	flight chan struct{}
}

func (c *remoteCache[T]) get(ctx context.Context, now func() time.Time, ttl time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	for {
		c.mu.Lock()
		if now().Before(c.exp) {
			v, err := c.val, c.err
			c.mu.Unlock()
			if err != nil {
				return zero, err
			}
			return v, nil
		}
		if c.flight == nil {
			c.flight = make(chan struct{})
			go c.fetch(now, ttl, fetch, c.flight, c.gen)
		}
		ch := c.flight
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

func (c *remoteCache[T]) fetch(now func() time.Time, ttl time.Duration, fetch func(context.Context) (T, error), done chan struct{}, gen int) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
	v, err := fetch(ctx)
	cancel()
	c.mu.Lock()
	if gen == c.gen {
		if err == nil {
			c.val, c.err, c.exp = v, nil, now().Add(ttl)
		} else {
			c.err, c.exp = err, now().Add(remoteErrTTL)
		}
	}
	c.flight = nil
	close(done)
	c.mu.Unlock()
}

// drop forgets the answer, so the next get reads anew.
func (c *remoteCache[T]) drop() {
	c.mu.Lock()
	c.exp, c.err = time.Time{}, nil
	c.gen++
	c.mu.Unlock()
}

var _ GatewayProvider = (*RemoteGateway)(nil)

func NewRemoteGateway(st store.Store, api func() *remotegw.Client) *RemoteGateway {
	return &RemoteGateway{st: st, api: api, now: time.Now}
}

// EnsureKit returns the node's kit, minting it on the provider the first time.
func (g *RemoteGateway) EnsureKit(ctx context.Context, nodeID uint) (store.RemoteKit, error) {
	if k, err := g.st.GetRemoteKit(nodeID); err != nil {
		return store.RemoteKit{}, err
	} else if k != nil {
		return *k, nil
	}
	node, err := g.st.GetNodeByID(int(nodeID))
	if err != nil {
		return store.RemoteKit{}, fmt.Errorf("node not found: %w", err)
	}
	api := g.api()
	minted, err := api.MintKit(ctx, kitName(node.Name), "")
	if err != nil {
		return store.RemoteKit{}, fmt.Errorf("mint link kit for node %d: %w", nodeID, err)
	}
	kit := store.RemoteKit{NodeID: nodeID, LinkID: minted.LinkID, LinkKey: minted.LinkKey}
	inserted, err := g.st.InsertRemoteKit(kit)
	if err != nil || !inserted {
		// Not stored, so nothing could ever use or revoke this kit later. Own
		// context: the caller's may be the one that just ran out.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if rerr := api.RevokeKit(rctx, minted.LinkID); rerr != nil && !remotegw.IsCode(rerr, "link_not_found") {
			log.Printf("[gateway-remote] revoke unused kit %s of node %d: %v", minted.LinkID, nodeID, rerr)
		}
		cancel()
	}
	if err != nil {
		return store.RemoteKit{}, err
	}
	if inserted {
		g.dropKitCache()
	}
	if !inserted {
		k, err := g.st.GetRemoteKit(nodeID)
		if err != nil {
			return store.RemoteKit{}, err
		}
		if k == nil {
			return store.RemoteKit{}, fmt.Errorf("link kit of node %d vanished while minting", nodeID)
		}
		return *k, nil
	}
	return kit, nil
}

func kitName(name string) string {
	name = strings.TrimSpace(name)
	for len(name) > remoteKitNameMax {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// RevokeKit revokes the node's kit on the provider, then forgets it.
func (g *RemoteGateway) RevokeKit(ctx context.Context, nodeID uint) error {
	k, err := g.st.GetRemoteKit(nodeID)
	if err != nil || k == nil {
		return err
	}
	if err := g.api().RevokeKit(ctx, k.LinkID); err != nil && !remotegw.IsCode(err, "link_not_found") {
		return fmt.Errorf("revoke link kit of node %d: %w", nodeID, err)
	}
	g.dropKitCache()
	return g.st.DeleteRemoteKit(nodeID)
}

func (g *RemoteGateway) dropKitCache() { g.kits.drop() }

// KitStatus maps node id -> the provider's view of its kit. The provider list
// is cached for 30 s, a failed call for 10 s.
func (g *RemoteGateway) KitStatus(ctx context.Context) (map[uint]remotegw.Kit, error) {
	kits, err := g.providerKits(ctx)
	if err != nil {
		return nil, err
	}
	local, err := g.st.ListRemoteKits()
	if err != nil {
		return nil, err
	}
	byLink := make(map[string]remotegw.Kit, len(kits))
	for _, k := range kits {
		byLink[k.LinkID] = k
	}
	out := make(map[uint]remotegw.Kit, len(local))
	for _, lk := range local {
		if k, ok := byLink[lk.LinkID]; ok {
			out[lk.NodeID] = k
		}
	}
	return out, nil
}

func (g *RemoteGateway) providerKits(ctx context.Context) ([]remotegw.Kit, error) {
	return g.kits.get(ctx, g.now, remoteKitsTTL, func(ctx context.Context) ([]remotegw.Kit, error) {
		return g.api().Kits(ctx)
	})
}

// ServerDomains lists the server's local remote_routes rows minus those with a
// delete still queued, so a deleted address leaves the server at once.
func (g *RemoteGateway) ServerDomains(serverUUID string) ([]string, error) {
	routes, err := g.serverRoutes(serverUUID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Domain)
	}
	return out, nil
}

// serverRoutes is ListRemoteRoutesByServer without the routes on their way out.
func (g *RemoteGateway) serverRoutes(serverUUID string) ([]store.RemoteRoute, error) {
	routes, err := g.st.ListRemoteRoutesByServer(serverUUID)
	if err != nil {
		return nil, err
	}
	out := routes[:0]
	for _, r := range routes {
		deleting, err := g.st.HasOpenOutbox(r.Domain, OutboxOpDelete)
		if err != nil {
			return nil, err
		}
		if !deleting {
			out = append(out, r)
		}
	}
	return out, nil
}

// ServerRoutes is the server's route list as GET /api/servers/{id}/routes
// shows it, from the local rows.
func (g *RemoteGateway) ServerRoutes(serverUUID string) ([]PublicRoute, error) {
	routes, err := g.serverRoutes(serverUUID)
	if err != nil {
		return nil, err
	}
	out := make([]PublicRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, PublicRoute{Domain: r.Domain, TargetIP: validate.ServerHostname(r.ServerUUID),
			TargetPort: r.Port, ServerUUID: r.ServerUUID, ProxyProtocol: r.ProxyProtocol})
	}
	return out, nil
}

// Regions is the provider's region and hoster-domain list, cached for 60 s, a
// failed call for 10 s.
func (g *RemoteGateway) Regions(ctx context.Context) (remotegw.Regions, error) {
	return g.regions.get(ctx, g.now, remoteRegionsTTL, func(ctx context.Context) (remotegw.Regions, error) {
		return g.api().Regions(ctx)
	})
}

// Usage is the provider's usage for the running month, cached for 60 s, a
// failed call for 10 s.
func (g *RemoteGateway) Usage(ctx context.Context) (remotegw.Usage, error) {
	return g.usage.get(ctx, g.now, remoteUsageTTL, func(ctx context.Context) (remotegw.Usage, error) {
		return g.api().Usage(ctx, "")
	})
}

// CheckDomain asks the provider whether domain is free; an answer is reused
// for 10 s, a failure is not.
func (g *RemoteGateway) CheckDomain(ctx context.Context, domain string) (available bool, reason string, err error) {
	g.checkMu.Lock()
	if a, ok := g.checks[domain]; ok && g.now().Before(a.exp) {
		g.checkMu.Unlock()
		return a.available, a.reason, nil
	}
	g.checkMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	available, reason, err = g.api().CheckDomain(ctx, domain)
	if err != nil {
		return false, "", err
	}
	g.checkMu.Lock()
	now := g.now()
	if g.checks == nil || len(g.checks) >= remoteCheckMax {
		// ponytail: dropped wholesale at the bound, not LRU; the entries live
		// 10 s and the route is rate limited per user.
		g.checks = map[string]checkAnswer{}
	}
	g.checks[domain] = checkAnswer{available: available, reason: reason, exp: now.Add(remoteCheckTTL)}
	g.checkMu.Unlock()
	return available, reason, nil
}

func kitCarriesClientAddr(k remotegw.Kit, found bool) bool {
	return found && k.Online != nil && *k.Online && slices.Contains(k.Caps, "client-addr")
}

// NodeKitCarriesClientAddr: hasKit is false while the node has no kit yet (the
// first route or move mints it); ok is true when the kit is online and
// announced client-addr.
func (g *RemoteGateway) NodeKitCarriesClientAddr(ctx context.Context, nodeID uint) (hasKit, ok bool, err error) {
	k, err := g.st.GetRemoteKit(nodeID)
	if err != nil || k == nil {
		return false, false, err
	}
	kits, err := g.KitStatus(ctx)
	if err != nil {
		return true, false, err
	}
	kit, found := kits[nodeID]
	return true, kitCarriesClientAddr(kit, found), nil
}

// ClientAddrGap reports how many routes the server has and the first of them
// whose node's kit is not online with client-addr ("" when every one is).
func (g *RemoteGateway) ClientAddrGap(ctx context.Context, serverUUID string) (routes int, gap string, err error) {
	rows, err := g.serverRoutes(serverUUID)
	if err != nil || len(rows) == 0 {
		return 0, "", err
	}
	kits, err := g.KitStatus(ctx)
	if err != nil {
		return len(rows), "", err
	}
	for _, r := range rows {
		if kit, found := kits[r.NodeID]; !kitCarriesClientAddr(kit, found) {
			return len(rows), r.Domain, nil
		}
	}
	return len(rows), "", nil
}

// SetNodesLinkOnline fills LinkOnline from the provider's kit list, one call
// for the whole list. A node without a kit, or a failed call, stays nil ("no
// answer"): the list itself must not fail because the provider did.
func (g *RemoteGateway) SetNodesLinkOnline(ctx context.Context, nodes []models.Node) {
	if len(nodes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	kits, err := g.KitStatus(ctx)
	if err != nil {
		log.Printf("[gateway-remote] link status for the node list: %v", err)
		return
	}
	for i := range nodes {
		if k, ok := kits[uint(nodes[i].ID)]; ok && k.Online != nil {
			v := *k.Online
			nodes[i].LinkOnline = &v
		}
	}
}

// ForgetNode drops a deleted node's kit and route rows. A revoke the provider
// did not take is queued (revoke_kit) and retried by the drain, so the local
// kit row can go at once: the node is gone and nothing else would read it.
func (g *RemoteGateway) ForgetNode(ctx context.Context, nodeID uint) {
	if err := g.RevokeKit(ctx, nodeID); err != nil {
		if k, _ := g.st.GetRemoteKit(nodeID); k != nil {
			if qerr := g.st.EnqueueOutbox(OutboxOpRevokeKit, k.LinkID, struct{}{}); qerr != nil {
				logErrf("gateway-remote", "node %d deleted, but the revoke of its link kit %s could not be queued; revoke it on the provider: %v (%v)", nodeID, k.LinkID, qerr, err)
			} else {
				log.Printf("[gateway-remote] node %d deleted: revoke of link kit %s queued after: %v", nodeID, k.LinkID, err)
			}
		}
		if derr := g.st.DeleteRemoteKit(nodeID); derr != nil {
			logErrf("gateway-remote", "node %d deleted, but its link kit row was not removed: %v", nodeID, derr)
		}
		g.dropKitCache()
	}
	routes, err := g.st.ListRemoteRoutes()
	if err != nil {
		logErrf("gateway-remote", "node %d deleted, but its route rows were not removed: %v", nodeID, err)
		return
	}
	dropped := 0
	for _, r := range routes {
		if r.NodeID != nodeID {
			continue
		}
		// A queued move re-homes the route and rewrites the row's node when it
		// lands; dropping the row first would leave that route unknown here.
		moving, err := g.st.HasOpenOutbox(r.Domain, OutboxOpMove)
		if err != nil || moving {
			log.Printf("[gateway-remote] node %d deleted: route row %s kept, a move is queued for it (%v)", nodeID, r.Domain, err)
			continue
		}
		if err := g.st.DeleteRemoteRoute(r.Domain); err != nil {
			logErrf("gateway-remote", "node %d deleted, but its route row %s was not removed: %v", nodeID, r.Domain, err)
			continue
		}
		dropped++
	}
	if dropped > 0 {
		log.Printf("[gateway-remote] node %d deleted: dropped %d route row(s)", nodeID, dropped)
	}
}

// ReportKitLeft reports, once per call, a deleted node's kit that could not be
// cleaned up because the gateway mode was unreadable. Link id only, never the
// key; nothing is reported for a node without a kit.
func (g *RemoteGateway) ReportKitLeft(nodeID uint) {
	if k, err := g.st.GetRemoteKit(nodeID); err == nil && k != nil {
		logErrf("gateway-remote", "node %d deleted, but its provider link kit %s was not cleaned up: the gateway provider setting could not be read; revoke it on the provider", nodeID, k.LinkID)
	}
}

// CreateServerRoute: ownerID is unused, every customer route meters under the
// provider API key's owner.
func (g *RemoteGateway) CreateServerRoute(serverID uint, ownerID string, domain string, port int) error {
	server, err := g.st.GetServerByID(int(serverID))
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}
	host := validate.ServerHostname(server.UUID)
	if host == "" {
		return ErrGatewayTargetName
	}
	if pending, err := g.st.HasOpenOutbox(domain, ""); err != nil {
		return err
	} else if pending {
		return ErrGatewayChangePending
	}
	// The row is claimed BEFORE the provider is asked: the provider sees one
	// account for every server here, so it would take a second server's put
	// as an edit and hand it the first server's players.
	node := uint(server.NodeID)
	claimed, held, err := g.st.ClaimRemoteRoute(store.RemoteRoute{Domain: domain, ServerUUID: server.UUID, NodeID: node, Port: port})
	if err != nil {
		return err
	}
	if !claimed && held.ServerUUID != server.UUID {
		return ErrGatewayDomainTaken
	}
	// Only a claim made by this call is taken back; a row that was already
	// there is the server's own, and its route stays.
	release := func() {
		if claimed {
			if derr := g.st.DeleteRemoteRoute(domain); derr != nil {
				logErrf("gateway-remote", "claim of %s not released after a failed create: %v", domain, derr)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
	defer cancel()
	kit, err := g.EnsureKit(ctx, node)
	if err != nil {
		release()
		return err
	}
	if err := g.api().PutRoute(ctx, kit.LinkID, domain, host, port); err != nil {
		release()
		return fmt.Errorf("create route %s: %w", domain, err)
	}
	if !claimed {
		// The same server creating its address again: the row follows the put.
		// Should this save fail, reconcile sees the node drift and moves it.
		held.NodeID, held.Port = node, port
		if err := g.st.UpsertRemoteRoute(*held); err != nil {
			return err
		}
	}
	// The local hub creates a route with the server's real-IP switch; the
	// provider's create has no such field, so a server that has it on gets it
	// through the outbox like any later flip. The route exists either way, so a
	// failed enqueue is reported, not returned.
	if server.ProxyProtocol {
		if err := g.st.EnqueueOutbox(OutboxOpProxy, domain, OutboxProxy{Enabled: true}); err != nil {
			logErrf("gateway-remote", "real-IP switch for new route %s not queued: %v", domain, err)
		}
	}
	return nil
}

// DeleteRoute queues the delete; the drain drops the remote_routes row once the
// provider confirmed, so a failing delete stays visible.
func (g *RemoteGateway) DeleteRoute(domain string) error {
	if queued, err := g.st.HasOpenOutbox(domain, OutboxOpDelete); err != nil || queued {
		return err
	}
	return g.st.EnqueueOutbox(OutboxOpDelete, domain, struct{}{})
}

func (g *RemoteGateway) DeleteServerRoutes(serverUUID string) error {
	if strings.TrimSpace(serverUUID) == "" {
		return nil
	}
	domains, err := g.ServerDomains(serverUUID)
	if err != nil {
		return err
	}
	for _, d := range domains {
		if err := g.DeleteRoute(d); err != nil {
			return err
		}
	}
	return nil
}

func (g *RemoteGateway) MigrateServerRoutes(serverID uint, newNodeID uint) error {
	server, err := g.st.GetServerByID(int(serverID))
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}
	host := validate.ServerHostname(server.UUID)
	if host == "" {
		return ErrGatewayTargetName
	}
	routes, err := g.serverRoutes(server.UUID)
	if err != nil || len(routes) == 0 {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
	defer cancel()
	kit, err := g.EnsureKit(ctx, newNodeID)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if err := g.st.EnqueueOutbox(OutboxOpMove, r.Domain, OutboxMove{LinkID: kit.LinkID, NodeID: newNodeID, TargetHost: host, Port: r.Port}); err != nil {
			return err
		}
	}
	return nil
}

func (g *RemoteGateway) SetServerProxyProtocol(serverUUID string, enabled bool) error {
	domains, err := g.ServerDomains(serverUUID)
	if err != nil {
		return err
	}
	for _, d := range domains {
		if err := g.st.EnqueueOutbox(OutboxOpProxy, d, OutboxProxy{Enabled: enabled}); err != nil {
			return err
		}
	}
	return nil
}

// Route-only and link switches belong to the provider's own tenants; a
// customer Core has none of them.

func (g *RemoteGateway) SetCoreOwnedRouteProxyProtocol(domain string, enabled bool) error {
	return ErrGatewayRemote
}

func (g *RemoteGateway) CreateRouteViaLink(ownerID string, domain string, linkToken string, targetHost string, port int) error {
	return ErrGatewayRemote
}

func (g *RemoteGateway) DeleteCoreOwnedRoute(domain string) error { return ErrGatewayRemote }

func (g *RemoteGateway) SuspendCoreOwnedRoute(domain, reason string) (string, error) {
	return "", ErrGatewayRemote
}

func (g *RemoteGateway) UnsuspendCoreOwnedRoute(domain string) (string, error) {
	return "", ErrGatewayRemote
}

func (g *RemoteGateway) SetNodeLinkEnabled(linkToken string, enabled bool) error {
	return ErrGatewayRemote
}

// LinkToken and DiscoveryProof have no remote equivalent: the token is derived
// from our cluster secret. Callers are gated in remote mode.
func (g *RemoteGateway) LinkToken(nodeID string) string      { return "" }
func (g *RemoteGateway) DiscoveryProof(nodeID string) string { return "" }
