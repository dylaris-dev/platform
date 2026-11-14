package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
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
	// OutboxOpDeleteCustomDomain releases the provider's claim on an own domain
	// (domain column = the domain, payload {}). It shares the domain's id
	// order, so a route delete queued before it reaches the provider first.
	OutboxOpDeleteCustomDomain = "delete_custom_domain"
	// OutboxOpVoice and OutboxOpBedrock replay an address's voice switch and
	// editions. Only a turn OFF, or a re-assert of the stored state, is queued:
	// an ON is sent at once, since the caller needs the port or the refusal.
	// The drain writes no local column; the caller already did.
	OutboxOpVoice   = "voice"
	OutboxOpBedrock = "bedrock"
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

// OutboxVoice is the payload of a "voice". TargetPort 0 is the provider's
// default (24454); a re-assert sends the port the address had.
type OutboxVoice struct {
	Enabled    bool `json:"enabled"`
	TargetPort int  `json:"targetPort,omitempty"`
}

// OutboxBedrock is the payload of a "bedrock": the address's editions.
type OutboxBedrock struct {
	Java    bool `json:"java"`
	Bedrock bool `json:"bedrock"`
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

// ErrGatewayKitRetiring refuses a new route, or a move, onto a node whose link
// kit is being replaced: the provider takes nothing new onto a retiring kit and
// revokes it, with every route still on it, at its deadline.
var ErrGatewayKitRetiring = errors.New("this node's link is being replaced; move the server to another node first")

// ErrGatewayKitKnown refuses to revoke, as unknown, a kit a node of this
// platform holds.
var ErrGatewayKitKnown = errors.New("this link belongs to a node of this platform")

// ErrGatewayNoKit: the node has no managed link yet, so there is nothing to
// replace.
var ErrGatewayNoKit = errors.New("this node has no managed link yet")

// ErrOwnDomainWritesBusy: this Core has sent ownDomainWritesPerMin own-domain
// claim writes to the provider in the running minute.
var ErrOwnDomainWritesBusy = errors.New("Too many own-domain changes on this platform right now. Try again in a minute.")

// ownDomainWritesPerMin bounds the own-domain claim writes (add, TXT value,
// verify, release) this process sends, for all users together: they share the
// platform's one provider key, and its write budget with every route change.
// With several replicas the ceiling is per replica. Route writes are not
// counted.
const ownDomainWritesPerMin = 12

const (
	remoteCallTimeout = 15 * time.Second
	remoteKitsTTL     = 30 * time.Second
	remoteRegionsTTL  = 60 * time.Second
	// The provider's usage figures change once a minute.
	remoteUsageTTL   = 60 * time.Second
	remoteAccountTTL = 60 * time.Second
	// The provider's claims on own domains; dropped after every claim write.
	remoteCustomDomainsTTL = 60 * time.Second
	// The provider's route list, for its voice and Bedrock state; dropped
	// after every voice or Bedrock write here.
	remoteRoutesTTL = 30 * time.Second
	// Player counts: 5 s keeps a platform at 12 reads a minute against the
	// key's 120.
	remotePlayersTTL = 5 * time.Second
	// A panel request waits this long for a player count; a slower provider
	// is answered from the last good count while its read goes on.
	remotePlayersWait = 2 * time.Second
	// The last good player counts are served this long after the read that
	// gave them (two edge sample periods), then nothing.
	remotePlayersStaleMax = 2 * time.Minute
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
	account remoteCache[remotegw.Account]
	domains remoteCache[map[string]remotegw.CustomDomain]
	routes  remoteCache[[]remotegw.Route]
	players remoteCache[remotegw.Players]
	// lastPlayers is the last count the provider answered (PlayerCounts),
	// lastPlayersAt when (this process's clock; zero: never).
	lastPlayersMu sync.Mutex
	lastPlayers   remotegw.Players
	lastPlayersAt time.Time

	checkMu sync.Mutex
	checks  map[string]checkAnswer

	// Own-domain claim writes in the window starting at claimWritesFrom.
	claimWritesMu   sync.Mutex
	claimWritesFrom time.Time
	claimWrites     int
}

// takeClaimWrite counts one own-domain claim write against the ceiling and
// reports whether it may be sent; when not, retryAt is the window's end. force
// counts a write that must follow one already sent (the TXT value after an add)
// without refusing it. ponytail: fixed one-minute window, so up to twice the
// ceiling can pass around a window edge.
func (g *RemoteGateway) takeClaimWrite(force bool) (ok bool, retryAt time.Time) {
	g.claimWritesMu.Lock()
	defer g.claimWritesMu.Unlock()
	now := g.now()
	if now.Sub(g.claimWritesFrom) >= time.Minute {
		g.claimWritesFrom, g.claimWrites = now, 0
	}
	if !force && g.claimWrites >= ownDomainWritesPerMin {
		return false, g.claimWritesFrom.Add(time.Minute)
	}
	g.claimWrites++
	return true, time.Time{}
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

// RetireNodeKit retires the node's kit on the provider and stamps the local
// row. The kit keeps working until the returned time, then the provider
// revokes it; meanwhile it no longer counts toward the host limit, so another
// node can get a kit. EnsureKit keeps returning the retiring kit for this
// node (never a second one) until reconcile forgets it after the deadline.
// Retiring a kit that is already retiring here answers its deadline again.
func (g *RemoteGateway) RetireNodeKit(ctx context.Context, nodeID uint) (time.Time, error) {
	k, err := g.st.GetRemoteKit(nodeID)
	if err != nil {
		return time.Time{}, err
	}
	if k == nil {
		return time.Time{}, ErrGatewayNoKit
	}
	if k.RetireAt != nil {
		return *k.RetireAt, nil
	}
	// Not the caller's context: a closed tab must not cut the call off between
	// the provider's commit and the local stamp.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteCallTimeout)
	defer cancel()
	api := g.api()
	at, err := api.RetireKit(ctx, k.LinkID)
	// An answer that never arrived (network, timeout, 5xx, 429) may hide a
	// retire the provider committed, and kit_already_retiring is one it did:
	// the provider's kit list is the truth then. A definite refusal is not.
	if err != nil && (remotegw.Retryable(err) || remotegw.IsCode(err, "kit_already_retiring")) {
		if pat, lerr := providerRetireAt(ctx, api, k.LinkID); lerr == nil {
			at, err = pat, nil
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("retire link kit of node %d: %w", nodeID, err)
	}
	g.dropKitCache()
	// A failed stamp is repaired by the sweep, which copies the provider's
	// deadline onto a row that has none.
	if err := g.st.SetRemoteKitRetireAt(nodeID, k.LinkID, at); err != nil {
		return time.Time{}, err
	}
	return at, nil
}

func providerRetireAt(ctx context.Context, api *remotegw.Client, linkID string) (time.Time, error) {
	kits, err := api.Kits(ctx)
	if err != nil {
		return time.Time{}, err
	}
	for _, k := range kits {
		if k.LinkID == linkID && k.RetireAt != nil {
			return *k.RetireAt, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: kit %s reported as retiring but listed without a deadline", remotegw.ErrBadAnswer, linkID)
}

// RetiringKitNodes is the set of nodes whose managed link is being replaced,
// in remote mode only (nil otherwise, and while the mode is unknown): automatic
// placement and moves skip them, since the provider takes no route onto such a
// link. One read serves a whole pass.
func RetiringKitNodes(st store.Store, mode func() (remote, known bool)) (map[int]bool, error) {
	if mode == nil {
		return nil, nil
	}
	if remote, known := mode(); !remote || !known {
		return nil, nil
	}
	kits, err := st.ListRemoteKits()
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, k := range kits {
		if k.RetireAt != nil {
			out[int(k.NodeID)] = true
		}
	}
	return out, nil
}

// NodeKitRetiring reports whether the node's kit is being replaced.
func (g *RemoteGateway) NodeKitRetiring(nodeID uint) (bool, error) {
	k, err := g.st.GetRemoteKit(nodeID)
	if err != nil || k == nil {
		return false, err
	}
	return k.RetireAt != nil, nil
}

// OrphanKits lists the provider's kits that no node here holds, from the
// cached kit list: what a lost or restored database leaves on the provider.
// Retiring kits are included and carry RetireAt. Another installation on the
// same provider account shows up here too; nothing on the provider tells the
// two apart.
func (g *RemoteGateway) OrphanKits(ctx context.Context) ([]remotegw.Kit, error) {
	kits, err := g.providerKits(ctx)
	if err != nil {
		return nil, err
	}
	known, err := g.localLinkIDs()
	if err != nil {
		return nil, err
	}
	out := []remotegw.Kit{}
	for _, k := range kits {
		if !known[k.LinkID] {
			out = append(out, k)
		}
	}
	return out, nil
}

func (g *RemoteGateway) localLinkIDs() (map[string]bool, error) {
	local, err := g.st.ListRemoteKits()
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(local))
	for _, k := range local {
		known[k.LinkID] = true
	}
	return known, nil
}

// RevokeOrphanKit revokes a provider kit no node here holds. The local rows
// are read again right before the call (the orphan list is cached), and a kit
// one of them names is refused with ErrGatewayKitKnown. A failure that may
// pass is queued as a revoke_kit (queued=true); a kit already gone is done.
func (g *RemoteGateway) RevokeOrphanKit(ctx context.Context, linkID string) (queued bool, err error) {
	known, err := g.localLinkIDs()
	if err != nil {
		return false, err
	}
	if known[linkID] {
		return false, ErrGatewayKitKnown
	}
	err = g.api().RevokeKit(ctx, linkID)
	switch {
	case err == nil || remotegw.IsCode(err, "link_not_found"):
		g.dropKitCache()
		return false, nil
	case remotegw.Retryable(err):
		if qerr := g.st.EnqueueOutbox(OutboxOpRevokeKit, linkID, struct{}{}); qerr != nil {
			return false, fmt.Errorf("revoke link kit %s: %w (queueing it failed: %v)", linkID, err, qerr)
		}
		return true, nil
	}
	return false, fmt.Errorf("revoke link kit %s: %w", linkID, err)
}

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

// Account is the provider's view of this platform's account, cached for 60 s,
// a failed call for 10 s.
func (g *RemoteGateway) Account(ctx context.Context) (remotegw.Account, error) {
	return g.account.get(ctx, g.now, remoteAccountTTL, func(ctx context.Context) (remotegw.Account, error) {
		return g.api().Account(ctx)
	})
}

// ProviderRoute is the provider's route for domain out of its route list,
// cached for 30 s (a failure for 10 s) and dropped after every voice or
// Bedrock write here. ok is false when the provider does not list it.
func (g *RemoteGateway) ProviderRoute(ctx context.Context, domain string) (remotegw.Route, bool, error) {
	routes, err := g.routes.get(ctx, g.now, remoteRoutesTTL, func(ctx context.Context) ([]remotegw.Route, error) {
		return g.api().Routes(ctx)
	})
	if err != nil {
		return remotegw.Route{}, false, err
	}
	for _, r := range routes {
		if r.Domain == domain {
			return r, true, nil
		}
	}
	return remotegw.Route{}, false, nil
}

// Players is the provider's player count for this platform's account, cached
// for 5 s, a failed call for 10 s.
func (g *RemoteGateway) Players(ctx context.Context) (remotegw.Players, error) {
	return g.players.get(ctx, g.now, remotePlayersTTL, func(ctx context.Context) (remotegw.Players, error) {
		return g.api().Players(ctx)
	})
}

// ProviderPlayers is what PlayerCounts answers: the counts, when they were
// read (zero: none) and whether they are an older copy served while the
// provider does not answer.
type ProviderPlayers struct {
	remotegw.Players
	At    time.Time
	Stale bool
}

// PlayerCounts is Players for a display: it waits at most remotePlayersWait,
// and a failed or slow read answers the last good counts, marked Stale, for
// at most remotePlayersStaleMax after that read (none before the first, none
// after). The provider fails the read whole on purpose, so zero would be a
// guess; a display only ever shows a count the provider gave.
func (g *RemoteGateway) PlayerCounts(ctx context.Context) ProviderPlayers {
	ctx, cancel := context.WithTimeout(ctx, remotePlayersWait)
	defer cancel()
	p, err := g.Players(ctx)
	g.lastPlayersMu.Lock()
	defer g.lastPlayersMu.Unlock()
	now := g.now()
	if err == nil {
		g.lastPlayers, g.lastPlayersAt = p, now
		return ProviderPlayers{Players: p, At: playersAt(p, now)}
	}
	if g.lastPlayersAt.IsZero() || now.Sub(g.lastPlayersAt) > remotePlayersStaleMax {
		return ProviderPlayers{}
	}
	return ProviderPlayers{Players: g.lastPlayers, At: playersAt(g.lastPlayers, g.lastPlayersAt), Stale: true}
}

// playersAt is when the counts were sampled: the provider's own time when it
// said, else when this process read them.
func playersAt(p remotegw.Players, read time.Time) time.Time {
	if !p.AsOf.IsZero() {
		return p.AsOf
	}
	return read
}

// ProviderCustomDomains is the provider's claim list keyed by domain: one call
// serves every domain, cached for 60 s (a failure for 10 s) and dropped after
// every claim write here. It is platform-wide: every user's domains, with the
// provider's TXT values. The map is the caller's copy.
func (g *RemoteGateway) ProviderCustomDomains(ctx context.Context) (map[string]remotegw.CustomDomain, error) {
	m, err := g.domains.get(ctx, g.now, remoteCustomDomainsTTL, func(ctx context.Context) (map[string]remotegw.CustomDomain, error) {
		items, err := g.api().CustomDomains(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]remotegw.CustomDomain, len(items))
		for _, d := range items {
			out[d.Domain] = d
		}
		return out, nil
	})
	return maps.Clone(m), err
}

// FreshProviderCustomDomains is ProviderCustomDomains read anew: the cache is
// per replica, and a claim written through another one is not in it yet.
func (g *RemoteGateway) FreshProviderCustomDomains(ctx context.Context) (map[string]remotegw.CustomDomain, error) {
	g.domains.drop()
	return g.ProviderCustomDomains(ctx)
}

// EnsureProviderClaim makes sure the provider holds a claim on domain with a
// TXT value, and returns it. A claim the cached list already shows with a
// value (or verified) is returned without a provider write; otherwise the
// provider's Add, which is idempotent. Refused with ErrGatewayChangePending
// while a release of the claim is still queued: the release would remove the
// claim made now.
func (g *RemoteGateway) EnsureProviderClaim(ctx context.Context, domain string) (remotegw.CustomDomain, error) {
	if pending, err := g.st.HasOpenOutbox(domain, OutboxOpDeleteCustomDomain); err != nil {
		return remotegw.CustomDomain{}, err
	} else if pending {
		return remotegw.CustomDomain{}, ErrGatewayChangePending
	}
	// A failed list read is not "no claim": the write below still answers.
	if claims, err := g.ProviderCustomDomains(ctx); err == nil {
		if d, ok := claims[domain]; ok && (d.TXTValue != "" || d.State == store.ClaimVerified) {
			return d, nil
		}
	}
	if ok, _ := g.takeClaimWrite(false); !ok {
		return remotegw.CustomDomain{}, ErrOwnDomainWritesBusy
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	defer g.domains.drop()
	api := g.api()
	d, err := api.AddCustomDomain(ctx, domain)
	if err != nil {
		return remotegw.CustomDomain{}, fmt.Errorf("claim %s on the gateway provider: %w", domain, err)
	}
	// A verified claim from before the provider kept its values has none, and
	// the provider refuses to issue one for a verified claim.
	if d.TXTValue == "" && d.State != store.ClaimVerified {
		g.takeClaimWrite(true)
		if d, err = api.IssueCustomDomainTXT(ctx, domain); err != nil {
			return remotegw.CustomDomain{}, fmt.Errorf("TXT value for %s from the gateway provider: %w", domain, err)
		}
	}
	return d, nil
}

// VerifyProviderClaim asks the provider to check its TXT value now; false
// with no error is "not found yet".
func (g *RemoteGateway) VerifyProviderClaim(ctx context.Context, domain string) (bool, error) {
	if ok, _ := g.takeClaimWrite(false); !ok {
		return false, ErrOwnDomainWritesBusy
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	defer g.domains.drop()
	return g.api().VerifyCustomDomainTXT(ctx, domain)
}

// ReleaseProviderClaim queues the release of the provider's claim on domain,
// unless a local claim of any user other than exceptUserID ("" excepts nobody)
// may still need it (pending or verified): every user here shares the one
// provider claim. exceptUserID is the user whose claim is going right now and
// still reads as live, as on a lapse, where the routes go before the claim.
//
// Nor while a route row on the domain stays: the release may only follow that
// row's queued delete (id order brings the delete to the provider first). A
// row with no delete queued, such as an operator server's route, still uses
// the claim. The drain finishes a release the provider refuses as in use, so
// a race past this check cannot hold the domain up.
func (g *RemoteGateway) ReleaseProviderClaim(domain, exceptUserID string) error {
	if n, err := g.st.CountLiveCustomDomainClaims(domain, exceptUserID); err != nil || n > 0 {
		return err
	}
	if row, err := g.st.GetRemoteRoute(domain); err != nil {
		return err
	} else if row != nil {
		if deleting, err := g.st.HasOpenOutbox(domain, OutboxOpDelete); err != nil || !deleting {
			return err
		}
	}
	if queued, err := g.st.HasOpenOutbox(domain, OutboxOpDeleteCustomDomain); err != nil || queued {
		return err
	}
	g.domains.drop()
	return g.st.EnqueueOutbox(OutboxOpDeleteCustomDomain, domain, struct{}{})
}

// IsCustomDomain reports whether domain is the customer's own, against the
// provider's hoster domains (Regions, cached).
func (g *RemoteGateway) IsCustomDomain(ctx context.Context, domain string) (bool, error) {
	regions, err := g.Regions(ctx)
	if err != nil {
		return false, err
	}
	return OwnDomain(domain, regions.HosterDomains), nil
}

// OwnDomain is true for a domain neither equal to nor under any hoster domain.
// A bare suffix match is not "under": evilplay.example.com is not under
// play.example.com.
func OwnDomain(domain string, hosters []remotegw.HosterDomain) bool {
	domain = normaliseHost(domain)
	for _, h := range hosters {
		base := normaliseHost(h.Domain)
		if base != "" && (domain == base || strings.HasSuffix(domain, "."+base)) {
			return false
		}
	}
	return true
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
	return g.createServerRoute(serverID, domain, port, nil)
}

// CreateServerRouteOwnDomain is CreateServerRoute for a caller that has already
// decided whether domain is an own one (and gated it as such), so the row's
// flag is that decision rather than a second read of the hoster domains.
func (g *RemoteGateway) CreateServerRouteOwnDomain(serverID uint, domain string, port int, custom bool) error {
	return g.createServerRoute(serverID, domain, port, &custom)
}

func (g *RemoteGateway) createServerRoute(serverID uint, domain string, port int, decided *bool) error {
	server, err := g.st.GetServerByID(int(serverID))
	if err != nil {
		return fmt.Errorf("server not found: %w", err)
	}
	host := validate.ServerHostname(server.UUID)
	if host == "" {
		return ErrGatewayTargetName
	}
	// Another server's address is taken whatever is queued for it; the
	// pending answer below is only for the server's own address. The claim
	// further down stays the authority when two creates race.
	if held, err := g.st.GetRemoteRoute(domain); err != nil {
		return err
	} else if held != nil && held.ServerUUID != server.UUID {
		return ErrGatewayDomainTaken
	}
	if pending, err := g.st.HasOpenOutbox(domain, ""); err != nil {
		return err
	} else if pending {
		return ErrGatewayChangePending
	}
	node := uint(server.NodeID)
	if retiring, err := g.NodeKitRetiring(node); err != nil {
		return err
	} else if retiring {
		return ErrGatewayKitRetiring
	}
	// Not knowing whether the domain is an own one is not "it is not": the
	// row's flag is what the reconcile checks the claims of.
	var custom bool
	if decided != nil {
		custom = *decided
	} else {
		rctx, rcancel := context.WithTimeout(context.Background(), remoteCallTimeout)
		custom, err = g.IsCustomDomain(rctx, domain)
		rcancel()
		if err != nil {
			return fmt.Errorf("create route %s: hoster domains: %w", domain, err)
		}
	}
	// The row is claimed BEFORE the provider is asked: the provider sees one
	// account for every server here, so it would take a second server's put
	// as an edit and hand it the first server's players.
	claimed, held, err := g.st.ClaimRemoteRoute(store.RemoteRoute{Domain: domain, ServerUUID: server.UUID, NodeID: node, Port: port, CustomDomain: custom})
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
	if custom {
		// The provider's route gate may just have proven its claim live; a
		// cached "pending" would have the sweep delete the route it let in.
		g.domains.drop()
	}
	if !claimed {
		// The same server creating its address again: the row follows the put.
		// Should this save fail, reconcile sees the node drift and moves it.
		held.NodeID, held.Port, held.CustomDomain = node, port, custom
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
	// The provider would refuse every move onto it; failing here lets the
	// move's cutover roll back instead of leaving the routes behind.
	if kit.RetireAt != nil {
		return ErrGatewayKitRetiring
	}
	for _, r := range routes {
		if err := g.st.EnqueueOutbox(OutboxOpMove, r.Domain, OutboxMove{LinkID: kit.LinkID, NodeID: newNodeID, TargetHost: host, Port: r.Port}); err != nil {
			return err
		}
	}
	return nil
}

// SetServerProxyProtocol queues the switch for every route of the server.
// Turning it ON asks the provider for the first route at once: a refusal there
// (its own real-IP feature off, a link without client-addr, a suspended route)
// would only repeat in the outbox every 10 minutes while the panel showed the
// switch as on, so it is returned and nothing is queued. A failure that may
// pass (network, 429, 5xx, no such route yet) is queued as before, and so is a
// route with a change already waiting, which must not be overtaken. Turning it
// OFF is only ever queued, never refused.
func (g *RemoteGateway) SetServerProxyProtocol(serverUUID string, enabled bool) error {
	domains, err := g.ServerDomains(serverUUID)
	if err != nil {
		return err
	}
	if enabled && len(domains) > 0 {
		first := domains[0]
		pending, err := g.st.HasOpenOutbox(first, "")
		if err != nil {
			return err
		}
		if !pending {
			ctx, cancel := context.WithTimeout(context.Background(), remoteCallTimeout)
			err := g.api().SetProxyProtocol(ctx, first, true)
			cancel()
			switch {
			case err == nil:
				if err := updateRemoteRoute(g.st, first, func(r *store.RemoteRoute) { r.ProxyProtocol = true }); err != nil {
					return err
				}
				domains = domains[1:]
			case !remotegw.Retryable(err) && !remotegw.IsCode(err, "not_found"):
				return fmt.Errorf("real-IP switch for %s: %w", first, err)
			}
		}
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
