package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"dylaris-core/store"
)

// warpStore is the narrow store surface the warp service needs (satisfied by
// store.Store and by test fakes).
type warpStore interface {
	GetWarpPeerByPubkey(pubkey string) (*store.WarpPeer, error)
	ListWarpPeersByKey(apiKeyID int) ([]store.WarpPeer, error)
	DeleteWarpPeerByPubkey(pubkey string) error
	ListWarpPeersByRegion(region string) ([]store.WarpPeer, error)
	CountWarpPeersByRegion() (map[string]int, error)
	EnrollPeerTx(keyID, limit int, onNewConn, pubkey, fixedIP, region string, allocIP func(taken map[string]bool) (string, error)) (wgIP string, evicted string, err error)
	ListWarpRegions() ([]store.WarpRegion, error)
	GetWarpRegion(region string) (*store.WarpRegion, error)
	ListWarpLeaders() ([]store.WarpLeader, error)
	SetWarpPeerAssignedLeader(pubkey, leaderID string) error
}

// ErrNoWarpRegion is returned by Enroll when no enabled region exists to assign.
var ErrNoWarpRegion = errors.New("no enabled warp region available")

// ErrNoWarpLeader is returned when the peer's region holds no enabled leader,
// so there is no endpoint to hand out.
//
// This has to be an ERROR rather than an empty endpoint list. Enrollment
// otherwise answers 200 with everything a client needs except the one address
// it dials: the client refuses its own config with "enroll response carried no
// leader endpoint" and retries every five seconds, forever, while Core's log
// says nothing at all. The region exists and the peer got an overlay address,
// so every screen reads healthy.
//
// Deliberately narrower than liveness. assignRegion degrades to a region whose
// leaders are all DOWN on purpose, because an endpoint that is not answering
// right now still works when the leader comes back. Zero enabled leader rows is
// the other thing: there is no address, and no amount of waiting produces one.
var ErrNoWarpLeader = errors.New("region has no enabled warp leader, so there is no tunnel endpoint to connect to")

// WarpService owns the warp registry side of Core. In the multi-hub model the
// REGION is the identity: a region owns a subnet and a WG key derived from
// CLUSTER_SECRET+region; its leaders are interchangeable endpoints. The service
// assigns peers to regions, allocates WG IPs from the region subnet, enforces
// connection policy, and fans peer commands out to every leader of the region.
type WarpService struct {
	warp          warpStore
	redis         *redis.Client
	clusterSecret string // for deriving region public keys (no heartbeat needed)
}

func NewWarpService(s warpStore, r *redis.Client, clusterSecret string) *WarpService {
	return &WarpService{warp: s, redis: r, clusterSecret: clusterSecret}
}

// leaderKeyHKDFSalt is the fixed domain-separation salt for the region/leader-key
// derivation. MUST stay byte-identical to gateway/warp/keys.go.
const leaderKeyHKDFSalt = "dylaris/warp/leader-key/v1"

// DeriveLeaderPublicKey reproduces the gateway warp DeriveLeaderKey derivation so
// Core knows a region's leader pubkey without a heartbeat. It expands the cluster
// secret with HKDF-SHA256 (fixed salt + the region id as info — all mirror leaders
// of a region share this key). MUST stay byte-identical to gateway/warp/keys.go.
func DeriveLeaderPublicKey(clusterSecret, region string) (string, error) {
	var km [32]byte
	r := hkdf.New(sha256.New, []byte(clusterSecret), []byte(leaderKeyHKDFSalt), []byte(region))
	if _, err := io.ReadFull(r, km[:]); err != nil {
		return "", fmt.Errorf("hkdf expand: %w", err)
	}
	k, err := wgtypes.NewKey(km[:])
	if err != nil {
		return "", fmt.Errorf("derive leader key: %w", err)
	}
	return k.PublicKey().String(), nil
}

// NextFreeIP returns the first usable host IP in subnet not in `taken`. The first
// host (.1 in a /24) is reserved for the leader. Network + broadcast are skipped.
func NextFreeIP(subnet string, taken map[string]bool) (string, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse subnet %q: %w", subnet, err)
	}
	base := ipnet.IP.Mask(ipnet.Mask).To4()
	if base == nil {
		return "", fmt.Errorf("only IPv4 subnets supported: %q", subnet)
	}
	cur := make(net.IP, len(base))
	copy(cur, base)
	leaderReserved := true
	for {
		incIP(cur)
		if !ipnet.Contains(cur) {
			return "", fmt.Errorf("subnet %s exhausted", subnet)
		}
		if leaderReserved {
			leaderReserved = false
			continue // skip the leader-reserved first host (e.g. .1)
		}
		next := make(net.IP, len(cur))
		copy(next, cur)
		incIP(next)
		if !ipnet.Contains(next) {
			return "", fmt.Errorf("subnet %s exhausted", subnet) // cur is broadcast
		}
		if !taken[cur.String()] {
			return cur.String(), nil
		}
	}
}

func incIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

// ErrInvalidFixedWGIP wraps every admin FixedWGIP validation failure so callers can
// map it to a client error (400/409) via errors.Is while surfacing the detail.
var ErrInvalidFixedWGIP = errors.New("invalid fixed WG IP")

// ErrWarpPeerNotFound is returned when an assignment poll names a pubkey that is
// not enrolled under the authenticated key.
var ErrWarpPeerNotFound = errors.New("warp peer not found")

// ValidateFixedWGIP checks that an admin-pinned fixed overlay IP is a legitimate peer
// host inside the region subnet: a valid IPv4 address in `subnet` that is not the
// network address, not the leader-reserved first host (network+1, e.g. .1), and not
// the broadcast address. This mirrors NextFreeIP's reserved-address semantics so a
// fixed IP is accepted iff NextFreeIP could have produced it. Every failure wraps
// ErrInvalidFixedWGIP.
func ValidateFixedWGIP(ip, subnet string) error {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("%w: parse subnet %q: %v", ErrInvalidFixedWGIP, subnet, err)
	}
	network := ipnet.IP.Mask(ipnet.Mask).To4()
	if network == nil {
		return fmt.Errorf("%w: only IPv4 subnets are supported: %q", ErrInvalidFixedWGIP, subnet)
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("%w: %q is not a valid IP", ErrInvalidFixedWGIP, ip)
	}
	ip4 := parsed.To4()
	if ip4 == nil {
		return fmt.Errorf("%w: %q is not an IPv4 address", ErrInvalidFixedWGIP, ip)
	}
	if !ipnet.Contains(ip4) {
		return fmt.Errorf("%w: %q is not inside region subnet %s", ErrInvalidFixedWGIP, ip, subnet)
	}
	if ip4.Equal(network) {
		return fmt.Errorf("%w: %q is the subnet network address", ErrInvalidFixedWGIP, ip)
	}
	leader := make(net.IP, len(network))
	copy(leader, network)
	incIP(leader)
	if ip4.Equal(leader) {
		return fmt.Errorf("%w: %q is the leader-reserved address (first host)", ErrInvalidFixedWGIP, ip)
	}
	broadcast := make(net.IP, len(network))
	for i := range network {
		broadcast[i] = network[i] | ^ipnet.Mask[i]
	}
	if ip4.Equal(broadcast) {
		return fmt.Errorf("%w: %q is the subnet broadcast address", ErrInvalidFixedWGIP, ip)
	}
	return nil
}

// EnrollResult mirrors the gateway client's enrollResponse JSON. Endpoints is the
// full failover list for the assigned region, alive-first; Endpoints[0] is the
// primary.
type EnrollResult struct {
	WGIP            string   `json:"wg_ip"`
	WGSubnet        string   `json:"wg_subnet"`
	Region          string   `json:"region"`
	LeaderPublicKey string   `json:"leader_public_key"`
	Endpoints       []string `json:"endpoints"`
	Assigned        bool     `json:"assigned"` // true when the peer has an explicit pinned home leader
	Keepalive       int      `json:"keepalive"`

	// Where the peer's local proxy forwards to. Filled by the HANDLER, not
	// here - resolving them means looking at Core's OWN networks, which is not
	// something this service can see.
	//
	// Empty means Core could not determine it; warp keeps whatever it had.
	CoreGRPCAddr string `json:"core_grpc_addr,omitempty"`
	RedisAddr    string `json:"redis_addr,omitempty"`
}

// --- Redis keys (all per-leader; queue/events already existed) ---

func (s *WarpService) leaderQueueKey(leaderID string) string {
	return "dylaris:warp:" + leaderID + ":queue"
}
func (s *WarpService) resyncKey(leaderID string) string {
	return "dylaris:warp:" + leaderID + ":resync-request"
}
func (s *WarpService) aliveKey(leaderID string) string {
	return "dylaris:warp:" + leaderID + ":alive"
}

func (s *WarpService) leaderAlive(ctx context.Context, leaderID string) bool {
	n, err := s.redis.Exists(ctx, s.aliveKey(leaderID)).Result()
	return err == nil && n > 0
}

// pushToRegion fans a command out to every enabled leader of the region. It is
// best-effort: a Redis push failure (or a region with no leader yet) does not fail
// the caller, because the per-leader resync covers a leader that missed a command.
func (s *WarpService) pushToRegion(ctx context.Context, region string, cmd map[string]interface{}) {
	leaders, err := s.warp.ListWarpLeaders()
	if err != nil {
		log.Printf("[warp] pushToRegion %s: list leaders: %v", region, err)
		return
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		log.Printf("[warp] pushToRegion %s: marshal: %v", region, err)
		return
	}
	for _, l := range leaders {
		if l.Region != region || !l.Enabled {
			continue
		}
		if err := s.redis.RPush(ctx, s.leaderQueueKey(l.LeaderID), b).Err(); err != nil {
			log.Printf("[warp] pushToRegion %s -> %s: %v", region, l.LeaderID, err)
		}
	}
}

// regionEndpoints returns the enabled leaders' endpoints for a region, freest
// first (see orderedRegionLeaderCands). Kept for callers that only need the
// plain failover list.
func (s *WarpService) regionEndpoints(ctx context.Context, region string) []string {
	cands := s.orderedRegionLeaderCands(ctx, region, "")
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.endpoint)
	}
	return out
}

// orderedRegionLeaderCands builds the region's enabled-leader candidates with
// liveness + F0 free-capacity telemetry, orders them freest-first, then moves the
// peer's home leader to the front when it is alive (home-first). An empty
// homeLeaderID yields the plain freest-first order. A non-nil empty slice is
// returned for an empty region (preserves the [] not null EnrollResult contract).
func (s *WarpService) orderedRegionLeaderCands(ctx context.Context, region, homeLeaderID string) []leaderCandidate {
	leaders, err := s.warp.ListWarpLeaders()
	if err != nil {
		return []leaderCandidate{}
	}
	var (
		cands     []leaderCandidate
		leaderIDs []string
	)
	for _, l := range leaders {
		if l.Region != region || !l.Enabled {
			continue
		}
		cands = append(cands, leaderCandidate{
			endpoint: l.Endpoint,
			id:       l.LeaderID,
			alive:    s.leaderAlive(ctx, l.LeaderID),
		})
		leaderIDs = append(leaderIDs, l.LeaderID)
	}
	gc := s.loadGatewayCapacity(ctx, leaderIDs)
	for i := range cands {
		cands[i].freeBps, cands[i].known = gc.freeBpsForLeader(cands[i].id)
	}
	sortLeaderCands(cands)
	moveHomeToFront(cands, homeLeaderID)
	if cands == nil {
		return []leaderCandidate{}
	}
	return cands
}

// assignRegion picks the region a new peer enrolls into: the key's preferred
// region if set + enabled, else the enabled region with the most aggregate free
// capacity among those with a live leader (peer-count is the tiebreak, and the
// pre-telemetry fallback). If no region has a live leader it degrades to the
// least-loaded enabled region (logged loudly) so enroll still succeeds and
// resync covers the hub later.
func (s *WarpService) assignRegion(ctx context.Context, key store.WarpAPIKey) (string, error) {
	regions, err := s.warp.ListWarpRegions()
	if err != nil {
		return "", err
	}
	enabled := make([]store.WarpRegion, 0, len(regions))
	for _, r := range regions {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return "", ErrNoWarpRegion
	}

	leaders, err := s.warp.ListWarpLeaders()
	if err != nil {
		return "", err
	}
	aliveByRegion := map[string]bool{}
	aliveLeaderIDsByRegion := map[string][]string{}
	// Regions that have an enabled leader AT ALL, alive or not. That is a
	// different question from liveness and it decides both the preference check
	// below and the fallback further down.
	hasLeaderByRegion := map[string]bool{}
	for _, l := range leaders {
		if !l.Enabled {
			continue
		}
		hasLeaderByRegion[l.Region] = true
		if s.leaderAlive(ctx, l.LeaderID) {
			aliveByRegion[l.Region] = true
			aliveLeaderIDsByRegion[l.Region] = append(aliveLeaderIDsByRegion[l.Region], l.LeaderID)
		}
	}

	// A key's stated preference is honoured only if the region can actually
	// carry a peer.
	//
	// "Enabled" alone was the test, which skipped the very check the placement
	// logic below applies deliberately - and its comment says why: a region with
	// no leader row "has no address and never will, so assigning a peer there is
	// choosing the one option that cannot recover". A key gets its region from
	// the create request with no leader check, and a region outlives its leaders
	// (removing a leader does not remove the region). Production had exactly that
	// pair: an enabled region left over from a hand-test, with a subnet, zero
	// leaders and zero peers, sitting next to the real one. A key naming it would
	// have enrolled "successfully" onto an overlay nothing programs.
	//
	// The bar is a leader ROW, not a live leader: a leader that is down comes
	// back, and refusing a preference during a restart would move peers off their
	// pinned region for no reason.
	if key.Region != "" {
		isEnabled := false
		for _, r := range enabled {
			if r.Region == key.Region {
				isEnabled = true
				break
			}
		}
		switch {
		case !isEnabled:
			log.Printf("[warp] key %d prefers region %q which is not enabled; auto-assigning", key.ID, key.Region)
		case !hasLeaderByRegion[key.Region]:
			log.Printf("[warp] key %d prefers region %q, which is enabled but has no leader; auto-assigning", key.ID, key.Region)
		default:
			return key.Region, nil
		}
	}
	counts, err := s.warp.CountWarpPeersByRegion()
	if err != nil {
		return "", err
	}

	regionsForPlacement := make([]store.WarpRegion, 0, len(enabled))
	for _, r := range enabled {
		if aliveByRegion[r.Region] {
			regionsForPlacement = append(regionsForPlacement, r)
		}
	}
	if len(regionsForPlacement) == 0 {
		// Degrade to regions that HAVE a leader, even a silent one. A leader
		// that is down still has an address and works again when it returns;
		// a region with no leader row has no address and never will, so
		// assigning a peer there is choosing the one option that cannot
		// recover. It matters most on a fresh install, where the seeded
		// default region has no leaders and would otherwise sit alongside a
		// real one and take assignments during any leader restart.
		for _, r := range enabled {
			if hasLeaderByRegion[r.Region] {
				regionsForPlacement = append(regionsForPlacement, r)
			}
		}
		if len(regionsForPlacement) > 0 {
			log.Printf("[warp] WARNING: no enabled region has a LIVE leader; assigning to the least-loaded region that has one")
		} else {
			// Nothing has a leader. Keep the historical behaviour rather than
			// failing the enroll here: Enroll refuses with ErrNoWarpLeader and
			// says which region, which is a better answer than "no region".
			log.Printf("[warp] WARNING: no enabled region has any leader; assigning to least-loaded enabled region anyway")
			regionsForPlacement = enabled
		}
	}

	// One mirror read for every alive leader across the candidate regions, then
	// rank regions by aggregate free capacity (peer-count is the tiebreak). With
	// no telemetry this degrades to the historical least-peer-count order.
	var allAliveIDs []string
	for _, r := range regionsForPlacement {
		allAliveIDs = append(allAliveIDs, aliveLeaderIDsByRegion[r.Region]...)
	}
	gc := s.loadGatewayCapacity(ctx, allAliveIDs)

	cands := make([]regionCandidate, 0, len(regionsForPlacement))
	for _, r := range regionsForPlacement {
		free, known := regionFreeBps(gc, aliveLeaderIDsByRegion[r.Region])
		cands = append(cands, regionCandidate{
			region:    r.Region,
			freeBps:   free,
			known:     known,
			peerCount: counts[r.Region],
		})
	}
	return pickFreestRegion(cands), nil
}

// buildResult assembles the enroll response for a peer already pinned to a region
// (used by both the new-enroll and idempotent paths so they can never disagree on
// subnet/key/endpoints — the bug the single-leader version had). homeLeaderID
// orders the peer's pinned F3 home leader to the front of Endpoints when it is
// alive; pass "" for the plain freest-first order.
func (s *WarpService) buildResult(ctx context.Context, region, wgIP, homeLeaderID string) (EnrollResult, error) {
	reg, err := s.warp.GetWarpRegion(region)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("region %q: %w", region, err)
	}
	pub, err := DeriveLeaderPublicKey(s.clusterSecret, region)
	if err != nil {
		return EnrollResult{}, err
	}
	cands := s.orderedRegionLeaderCands(ctx, region, homeLeaderID)
	endpoints := make([]string, 0, len(cands))
	for _, c := range cands {
		endpoints = append(endpoints, c.endpoint)
	}
	if len(endpoints) == 0 {
		return EnrollResult{}, fmt.Errorf("region %q: %w", region, ErrNoWarpLeader)
	}
	return EnrollResult{
		WGIP:            wgIP,
		WGSubnet:        reg.Subnet,
		Region:          region,
		LeaderPublicKey: pub,
		Endpoints:       endpoints,
		Assigned:        homeLeaderID != "",
		Keepalive:       25,
	}, nil
}

// Enroll enforces the key's policy, assigns + persists the peer to a region,
// allocates a WG IP from the region subnet, fans add_peer (plus remove_peer for
// kill_old) out to every leader of the region, and returns the full tunnel config.
func (s *WarpService) Enroll(ctx context.Context, key store.WarpAPIKey, pubkey string, _ []string) (EnrollResult, error) {
	// Idempotent: same pubkey already enrolled -> rebuild its config from its
	// stored region, no new push.
	if existing, err := s.warp.GetWarpPeerByPubkey(pubkey); err == nil && existing != nil {
		return s.buildResult(ctx, existing.Region, existing.WGIP, existing.AssignedLeader)
	}

	region, err := s.assignRegion(ctx, key)
	if err != nil {
		return EnrollResult{}, err
	}
	reg, err := s.warp.GetWarpRegion(region)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("region %q: %w", region, err)
	}

	// A caller-pinned fixed IP must be a legitimate host inside the region subnet.
	// Authoritative gate: also catches keys minted before this check and post-mint
	// subnet changes. Empty FixedWGIP (auto-allocation) is unaffected.
	if key.FixedWGIP != "" {
		if verr := ValidateFixedWGIP(key.FixedWGIP, reg.Subnet); verr != nil {
			return EnrollResult{}, verr
		}
	}

	limit := key.MaxConns
	if key.Policy == "fixed" {
		limit = 1
	}

	// Atomic: policy enforcement + IP allocation + insert under one tx + advisory lock.
	wgIP, evicted, err := s.warp.EnrollPeerTx(key.ID, limit, key.OnNewConn, pubkey, key.FixedWGIP, region,
		func(taken map[string]bool) (string, error) { return NextFreeIP(reg.Subnet, taken) })
	if err != nil {
		if errors.Is(err, store.ErrWarpLimitReached) {
			return EnrollResult{}, fmt.Errorf("%w (policy=%s, max=%d)", store.ErrWarpLimitReached, key.Policy, limit)
		}
		return EnrollResult{}, err
	}

	// Fan out leader commands after the DB commit (best-effort; resync covers a
	// leader that was down). Evicted peer (kill_old) is removed from every mirror.
	if evicted != "" {
		s.pushToRegion(ctx, region, map[string]interface{}{"type": "remove_peer", "pubkey": evicted})
	}
	s.pushToRegion(ctx, region, map[string]interface{}{"type": "add_peer", "pubkey": pubkey, "allowed_ips": []string{wgIP + "/32"}})

	// F3: pin a fresh peer's home to the region's freest leader so it does not
	// drift on later re-enrolls; best-effort (an unpinned peer just falls back to
	// freest-first, which is the pre-F3 behavior).
	home := ""
	if cands := s.orderedRegionLeaderCands(ctx, region, ""); len(cands) > 0 {
		home = cands[0].id
		if err := s.warp.SetWarpPeerAssignedLeader(pubkey, home); err != nil {
			log.Printf("[warp] pin home for %s failed: %v", pubkey, err)
			home = "" // fall back to freest-first for the response too
		}
	}
	return s.buildResult(ctx, region, wgIP, home)
}

// Assignment returns the current home-first tunnel config for one enrolled peer,
// used by the client's lightweight assignment poll. The peer must belong to the
// authenticated key. Region is never changed here (region-change is a separate
// mechanism); this only reflects the peer's stored home leader.
func (s *WarpService) Assignment(ctx context.Context, key store.WarpAPIKey, pubkey string) (EnrollResult, error) {
	peer, err := s.warp.GetWarpPeerByPubkey(pubkey)
	if err != nil || peer == nil {
		return EnrollResult{}, ErrWarpPeerNotFound
	}
	if peer.APIKeyID != key.ID {
		return EnrollResult{}, ErrWarpPeerNotFound
	}
	return s.buildResult(ctx, peer.Region, peer.WGIP, peer.AssignedLeader)
}

// DisconnectKeyPeers removes every peer enrolled under one API key: the WG peer
// is dropped from each leader of its region and the row is deleted.
//
// Revoking a key alone only blocks the NEXT enroll - an already-established
// tunnel keeps forwarding, because WireGuard has no notion of the key that
// created it. Without this, "revoke" in the panel would leave a live overlay
// member behind and read as a security control that is not one.
//
// Best-effort per peer and per leader: a leader that is down misses the command
// but converges on its next resync, which rebuilds the peer set from the DB
// rows this function has already deleted. Returns how many peers were removed.
func (s *WarpService) DisconnectKeyPeers(ctx context.Context, keyID int) int {
	// A nil service is a state callers legitimately hold (no overlay wired), and
	// they hand it over as an INTERFACE - where a nil pointer is not nil to a nil
	// check. Guarding here rather than at each call site is what keeps that from
	// being a panic in the account delete.
	if s == nil || s.warp == nil {
		return 0
	}
	peers, err := s.warp.ListWarpPeersByKey(keyID)
	if err != nil {
		log.Printf("warp: list peers for key %d: %v", keyID, err)
		return 0
	}
	removed := 0
	for _, p := range peers {
		s.pushToRegion(ctx, p.Region, map[string]interface{}{"type": "remove_peer", "pubkey": p.Pubkey})
		if derr := s.warp.DeleteWarpPeerByPubkey(p.Pubkey); derr != nil {
			log.Printf("warp: delete peer %s: %v", p.Pubkey, derr)
			continue
		}
		removed++
	}
	return removed
}

// --- Resync ---

// processResync iterates the enabled leaders and, for any with a pending
// per-leader resync-request, pushes that leader's full region peer set as a
// `resync` command then clears the request. Per-leader keys mean mirror leaders
// no longer clobber each other (the single-key bug of the single-hub version).
func (s *WarpService) processResync(ctx context.Context) error {
	leaders, err := s.warp.ListWarpLeaders()
	if err != nil {
		return err
	}
	for _, l := range leaders {
		if !l.Enabled {
			continue
		}
		_, err := s.redis.Get(ctx, s.resyncKey(l.LeaderID)).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return err
		}
		peers, err := s.warp.ListWarpPeersByRegion(l.Region)
		if err != nil {
			return err
		}
		specs := make([]map[string]interface{}, 0, len(peers))
		for _, p := range peers {
			specs = append(specs, map[string]interface{}{
				"pubkey": p.Pubkey, "wg_ip": p.WGIP, "allowed_ips": []string{p.WGIP + "/32"},
			})
		}
		b, err := json.Marshal(map[string]interface{}{"type": "resync", "peers": specs})
		if err != nil {
			return err
		}
		if err := s.redis.RPush(ctx, s.leaderQueueKey(l.LeaderID), b).Err(); err != nil {
			return err
		}
		if err := s.redis.Del(ctx, s.resyncKey(l.LeaderID)).Err(); err != nil {
			return err
		}
	}
	return nil
}

// StartResyncWatcher runs processResync on a ticker. isLeader gates it so only the
// elected Core acts.
func (s *WarpService) StartResyncWatcher(isLeader func() bool) {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			if isLeader != nil && !isLeader() {
				continue
			}
			if err := s.processResync(context.Background()); err != nil {
				log.Printf("[warp] resync watcher: %v", err)
			}
		}
	}()
}

// --- Registry overview (for the panel) ---

type LeaderView struct {
	LeaderID string `json:"leaderId"`
	Endpoint string `json:"endpoint"`
	Enabled  bool   `json:"enabled"`
	Alive    bool   `json:"alive"`
}

type RegionView struct {
	Region    string       `json:"region"`
	Subnet    string       `json:"subnet"`
	Enabled   bool         `json:"enabled"`
	PeerCount int          `json:"peerCount"`
	Leaders   []LeaderView `json:"leaders"`
}

// RegionsOverview returns the full registry (regions + their leaders + liveness +
// peer counts) for the admin panel in one shot.
func (s *WarpService) RegionsOverview(ctx context.Context) ([]RegionView, error) {
	regions, err := s.warp.ListWarpRegions()
	if err != nil {
		return nil, err
	}
	leaders, err := s.warp.ListWarpLeaders()
	if err != nil {
		return nil, err
	}
	counts, err := s.warp.CountWarpPeersByRegion()
	if err != nil {
		return nil, err
	}
	byRegion := map[string][]LeaderView{}
	for _, l := range leaders {
		byRegion[l.Region] = append(byRegion[l.Region], LeaderView{
			LeaderID: l.LeaderID, Endpoint: l.Endpoint, Enabled: l.Enabled,
			Alive: s.leaderAlive(ctx, l.LeaderID),
		})
	}
	out := make([]RegionView, 0, len(regions))
	for _, r := range regions {
		out = append(out, RegionView{
			Region: r.Region, Subnet: r.Subnet, Enabled: r.Enabled,
			PeerCount: counts[r.Region], Leaders: byRegion[r.Region],
		})
	}
	return out, nil
}

// --- Region subnet mirror (Redis) ---

// warpRegionSubnetKey is the fixed key Core mirrors a region's subnet to so a warp
// leader can read its authoritative subnet at boot. MUST stay byte-identical to the
// gateway warp leader's reader (cross-repo contract; no version byte).
func warpRegionSubnetKey(region string) string {
	return "dylaris:warp:region:" + region + ":subnet"
}

// PublishRegionSubnet mirrors a region's subnet to Redis. Authoritative source for
// the warp leader's gateway IP / route / NAT. Idempotent; TTL 0 (config, not liveness).
func PublishRegionSubnet(ctx context.Context, rdb *redis.Client, region, subnet string) error {
	return rdb.Set(ctx, warpRegionSubnetKey(region), subnet, 0).Err()
}
