package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type DiscoveryService struct {
	store         store.Store
	redis         *redis.Client
	clusterSecret string
	// leader is set by main.go after construction. nil = run unconditionally
	// (single-Core dev mode); non-nil = only run when this Core holds the
	// global lease. See pkg/leader.
	leader leader.Election
	// badRegionReported remembers which unknown NODE_REGION value was last
	// reported per node, so a misconfigured node is announced once instead of
	// every 5 seconds. The error stream is capped at 500 entries and trims
	// itself, so a repeating report would evict every other error in about
	// forty minutes. Only scanNodes touches it, from the single ticker
	// goroutine in Start, so it needs no lock.
	badRegionReported map[int]string
}

// SetLeader wires the leader-election gate. Call once at boot.
func (s *DiscoveryService) SetLeader(l leader.Election) { s.leader = l }

// Payload that the Node writes to Redis
type NodeHeartbeat struct {
	ID            string                 `json:"id"`            // Unique ID (e.g. hostname)
	Name          string                 `json:"name"`          // Display name
	IP            string                 `json:"ip"`            // IP for display
	ClusterSecret string                 `json:"clusterSecret"` // For validation
	Tags          string                 `json:"tags"`
	Region        string                 `json:"region"` // NODE_REGION env on the node, e.g. "eu-central"
	IPs           NodeIPs                `json:"ips"`
	CPUUsage      float64                `json:"cpuUsage"`
	RAMFree       int64                  `json:"ramFree"`
	RAMTotal      uint64                 `json:"ramTotal"`
	TotalCPU      float64                `json:"totalCpu"`
	Storage       []HeartbeatStoragePath `json:"storage"`
	// LinkCount is how many link containers the node runs. The node has always
	// written it (main.go); this struct did not declare it, so the panel handler
	// read the same key through a private duplicate of this type just to see it.
	// A POINTER for the same reason as NetPolicy: a heartbeat without it is
	// unknown, and the panel warns about a 0.
	LinkCount *int `json:"linkCount,omitempty"`
	// PortRange is the node's effective MC host-port range ("25600-25699") and
	// PortRangeNotice is set only when the node fell back to its default because
	// PORT_RANGE was unset or unparseable. Surfaced so an admin sees the ports
	// the host firewall must open, and sees a typo instead of a silent default.
	PortRange       string `json:"portRange,omitempty"`
	PortRangeNotice string `json:"portRangeNotice,omitempty"`
	// NetPolicy is whether this node refuses server-to-server traffic that
	// nothing allowed, NetPolicyServers how many of its servers currently carry
	// the rules, and NetPolicyNotice the one sentence worth showing about it.
	// A POINTER: an older node reports nothing, and nothing is not "off". See
	// models.Node.NetPolicy.
	NetPolicy        *bool  `json:"netPolicy,omitempty"`
	NetPolicyServers int    `json:"netPolicyServers,omitempty"`
	NetPolicyNotice  string `json:"netPolicyNotice,omitempty"`
	// SharedStorage is non-empty when the node found one of its storage paths
	// mounted into another node too. That topology cannot work - node identity
	// lives in the first storage path - and it silently destroys a server on the
	// next migration, so it is carried all the way to the panel.
	SharedStorage []models.SharedStorageConflict `json:"sharedStorage,omitempty"`
	// ReleaseVersion is the release this node's IMAGE was built from, stamped in
	// at build time. It is what lets the panel say whether the NODE is behind,
	// rather than assuming it moved whenever Core did - an operator who updates
	// Core and leaves the nodes alone was previously told the node's changes were
	// installed. Empty means "this build does not report one", which the reader
	// must treat as unknown, never as very old.
	//
	// It is reported PER NODE and read per node. There used to be a fleet-wide
	// minimum in Redis, which cannot express "two of your three nodes are
	// current" - the case an operator most needs to see.
	ReleaseVersion string `json:"releaseVersion,omitempty"`
	// Link sidecar image state, reported only by nodes that manage their own Link.
	// LinkManaged distinguishes "this node has no Link to update" from "this node
	// runs an operator-deployed Link", so the panel does not offer a button that
	// would do nothing. Empty image ids mean unknown (no container yet, or the
	// registry was unreachable) and must not be read as an update being available.
	LinkManaged         bool   `json:"linkManaged,omitempty"`
	LinkImageRunning    string `json:"linkImageRunning,omitempty"`
	LinkImageAvailable  string `json:"linkImageAvailable,omitempty"`
	LinkUpdateAvailable bool   `json:"linkUpdateAvailable,omitempty"`
	// Timestamp (unix seconds) + Sig replace the raw-secret compare on the
	// hardened path: Sig = HMAC(perNodeSecret, heartbeat-domain|token|ts).
	Timestamp int64  `json:"timestamp"`
	Sig       string `json:"sig,omitempty"`
}

// HeartbeatStoragePath is one storage path reported by the node.
type HeartbeatStoragePath struct {
	Path        string `json:"path"`
	TotalBytes  int64  `json:"total_bytes"`
	FreeBytes   int64  `json:"free_bytes"`
	UsedBytes   int64  `json:"used_bytes"`
	ServerCount int    `json:"server_count"`
	// ServerUUIDs are the top-level directories the node found on this path.
	// It is what lets Core attribute each server's PROMISED disk limit to the
	// path it actually lives on - free space alone cannot show commitment.
	ServerUUIDs []string `json:"server_uuids"`
	// QuotaEnforceable reports whether a per-server disk limit can actually be
	// enforced here (project quotas need xfs/ext4). Surfaced so a limit that
	// silently does nothing on NFS/CIFS is visible instead of assumed.
	QuotaEnforceable bool `json:"quota_enforceable"`
}

// LoadHeartbeats reads every node heartbeat currently in Redis and
// returns a map keyed by node token (= heartbeat ID). Used by callers
// that need to inspect more than one node at a time (scheduler, infra
// overview, capacity sync).
func LoadHeartbeats(ctx context.Context, r *redis.Client) map[string]*NodeHeartbeat {
	out := map[string]*NodeHeartbeat{}
	if r == nil {
		return out
	}
	keys, err := r.Keys(ctx, "dylaris:discovery:*").Result()
	if err != nil {
		return out
	}
	for _, key := range keys {
		val, err := r.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		var hb NodeHeartbeat
		if err := json.Unmarshal([]byte(val), &hb); err != nil {
			continue
		}
		out[hb.ID] = &hb
	}
	return out
}

// LoadHeartbeat reads ONE node's heartbeat by token. Prefer it over
// LoadHeartbeats when a single node is wanted: the plural form does a KEYS scan
// over the whole fleet.
func LoadHeartbeat(ctx context.Context, r *redis.Client, nodeToken string) *NodeHeartbeat {
	if r == nil || nodeToken == "" {
		return nil
	}
	val, err := r.Get(ctx, "dylaris:discovery:"+nodeToken).Result()
	if err != nil || val == "" {
		return nil
	}
	var hb NodeHeartbeat
	if json.Unmarshal([]byte(val), &hb) != nil {
		return nil
	}
	return &hb
}

type NodeIPs struct {
	Public  string   `json:"public"`
	Private []string `json:"private"`
}

func NewDiscoveryService(s store.Store, r *redis.Client, secret string) *DiscoveryService {
	return &DiscoveryService{
		store:             s,
		redis:             r,
		clusterSecret:     secret,
		badRegionReported: map[int]string{},
	}
}

// publishServersChanged drops one servers.changed event into the system-events
// channel so the panel re-fetches the server list (which carries each server's
// node status). Inlined like the status watcher's copy so DiscoveryService needs
// no publisher dependency. Best-effort: a publish error is logged, never fatal.
func (s *DiscoveryService) publishServersChanged(ctx context.Context) {
	payload, err := json.Marshal(SystemEvent{Type: "servers.changed"})
	if err != nil {
		return
	}
	if err := s.redis.Publish(ctx, SystemEventsChannel, payload).Err(); err != nil {
		log.Printf("discovery: publish servers.changed failed: %v", err)
	}
}

func (s *DiscoveryService) Start() {
	log.Println("Discovery Service started (Auto-Discovery active)")

	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			// Leader-gate: only the elected Core scans + writes node rows.
			// Followers idle through each tick. nil leader = run anyway
			// (covers tests + single-instance dev where Redis-leader isn't wired).
			if s.leader != nil && !s.leader.IsLeader() {
				continue
			}
			s.scanNodes()
		}
	}()
}

// checkCPUTopologyChange compares the node's currently reported CPU topology
// against the last-seen signature (kept in Redis). On a real hardware change it
// resets every CPU pinning on the node (server cpusets + node cpuset). The first
// observation just records the signature (no reset). Runs only on the leader
// because scanNodes is leader-gated.
func (s *DiscoveryService) checkCPUTopologyChange(ctx context.Context, node *models.Node) {
	raw, err := s.redis.Get(ctx, fmt.Sprintf("dylaris:node:%s:cpu", node.Token)).Result()
	if err != nil {
		return // node has not reported a topology
	}
	var topo CPUTopology
	if err := json.Unmarshal([]byte(raw), &topo); err != nil {
		return
	}
	sig := TopologySignature(&topo)
	if sig == "" {
		return
	}
	sigKey := fmt.Sprintf("dylaris:node:%s:cpu:sig", node.Token)
	prev, _ := s.redis.Get(ctx, sigKey).Result()
	if prev == sig {
		return
	}
	if prev != "" {
		resetN, rerr := s.store.ResetServerCPUPinningByNode(node.ID)
		if rerr != nil {
			log.Printf("CPU topology changed on %s but pinning reset failed: %v", node.Name, rerr)
			return // keep the old signature so the reset is retried next scan
		}
		if cerr := s.store.UpdateNodeCpusetCpus(node.ID, ""); cerr != nil {
			log.Printf("CPU topology changed on %s: node cpuset reset failed: %v", node.Name, cerr)
		}
		log.Printf("Node %s CPU topology changed -> reset %d server pinning(s) + node cpuset", node.Name, resetN)
	}
	s.redis.Set(ctx, sigKey, sig, 0)
}

// NodeLinkStateKey holds a nodeID -> NodeLinkState map for the nodes that manage
// their own Link sidecar. Published by the discovery sweep so the panel can show
// pending Link updates without a DB column: the value is live state with a TTL,
// not something worth a migration.
const NodeLinkStateKey = "dylaris:nodes:link_state"

// NodeLinkState is one node's Link sidecar image status.
type NodeLinkState struct {
	// Managed is false for a node whose Link an operator deploys. The panel must
	// not offer an update button there - Core cannot replace that container.
	Managed         bool   `json:"managed"`
	Running         string `json:"running,omitempty"`
	Available       string `json:"available,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

// applyHeartbeatRegion stores an auto-discovered node's NODE_REGION, but
// only once it names a region that EXISTS - a region an operator has created
// when an admin adopts a node by hand.
//
// It has to be the same rule because this column is a join key, not a label.
// It picks the beam relay (a HARD filter, so the wrong value sends a transfer
// across an ocean), it is what stops the rebalancer moving a server between
// regions, it decides which servers regional staff may see, and it is copied
// onto every server created on this node - where CountServersInRegion reads it
// to decide whether a region may be deleted. A NODE_REGION typo used to be
// written through unchecked, and each of those consumers then answered about a
// region no row describes: staff silently lost sight of the servers, and the
// delete guard counted zero for a region that was really in use.
//
// Region ids are canonically lowercase - CreateRegion lowercases and the id
// regex allows nothing else - so normalise before the lookup. Otherwise
// NODE_REGION=EU would be refused over its casing while naming a region
// that plainly exists.
func (s *DiscoveryService) applyHeartbeatRegion(node *models.Node, reported string) {
	region := strings.ToLower(strings.TrimSpace(reported))
	if region == "" {
		return
	}
	if _, err := s.store.GetRegion(region); err != nil {
		// Once per distinct bad value, not once per 5s tick: see badRegionReported.
		if s.badRegionReported[node.ID] != region {
			s.badRegionReported[node.ID] = region
			logErrf("discovery", "node %s reports region %q, which is not a configured region - keeping %q. Create the region or fix NODE_REGION on that node.",
				node.Name, reported, node.Region)
		}
		return
	}
	delete(s.badRegionReported, node.ID)
	if region == node.Region {
		return
	}
	log.Printf("Node region updated for %s: %q -> %q", node.Name, node.Region, region)
	s.store.SetNodeRegion(node.ID, region)
}

// syncNodeMetadataFromHeartbeat applies the two fields a node supplies through
// its own environment: NODE_TAGS and NODE_REGION.
//
// An empty value is the node saying NOTHING, not saying "none", so it leaves
// the column alone. A node that never sets NODE_REGION keeps whatever region it
// has rather than being blanked on every beat.
func (s *DiscoveryService) syncNodeMetadataFromHeartbeat(node *models.Node, hb NodeHeartbeat) {
	// Say it out loud the first time the environment takes a field back off a
	// node that was configured by hand, so the change of ownership is something
	// an operator can read rather than discover.
	if hb.Tags != "" && node.Tags != hb.Tags {
		if node.Configured {
			log.Printf("Node %s: NODE_TAGS now wins over the value set in the panel (%q -> %q)",
				node.Name, node.Tags, hb.Tags)
		} else {
			log.Printf("Node Tags updated for %s: %s", node.Name, hb.Tags)
		}
		s.store.SetNodeTags(node.ID, hb.Tags)
	}

	if hb.Region != "" {
		if node.Configured && node.Region != hb.Region {
			log.Printf("Node %s: NODE_REGION now wins over the value set in the panel (%q -> %q)",
				node.Name, node.Region, hb.Region)
		}
		s.applyHeartbeatRegion(node, hb.Region)
	}
}

func (s *DiscoveryService) scanNodes() {
	ctx := context.Background()

	// 1. Find all Discovery Keys: dylaris:discovery:*
	keys, err := s.redis.Keys(ctx, "dylaris:discovery:*").Result()
	if err != nil {
		log.Printf("Discovery Redis Error: %v", err)
		return
	}

	activeNodeTokens := make(map[string]bool)
	linkStates := map[string]NodeLinkState{}

	for _, key := range keys {
		val, err := s.redis.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		var hb NodeHeartbeat
		if err := json.Unmarshal([]byte(val), &hb); err != nil {
			continue
		}

		// 2. Security Check. Redis ACL is mandatory: the raw cluster secret is never
		// on the wire. A new node is created only via the gRPC enroll path (never from
		// a heartbeat), and an existing node's per-node HMAC signature is verified in
		// the existing-node branch below.

		// 3. Find or create Node in DB
		activeNodeTokens[hb.ID] = true
		if hb.LinkManaged {
			linkStates[hb.ID] = NodeLinkState{
				Managed:         true,
				Running:         hb.LinkImageRunning,
				Available:       hb.LinkImageAvailable,
				UpdateAvailable: hb.LinkUpdateAvailable,
			}
		}

		node, err := s.store.GetNodeByToken(hb.ID)

		if err != nil {
			// Node does not exist -> Create new!

			// Server-assigned identity: gRPC enroll (gated by a single-use enroll
			// token) is the ONLY node-creation path. A heartbeat for an id the Core
			// never assigned must NOT auto-register a node, or the Core-minted identity
			// would be trivially bypassable. Liveness updates for already-enrolled
			// nodes (the else branch) are unaffected.
			continue
		} else {
			now := time.Now().Unix()
			secret, ok, lerr := redisacl.LoadNodeSecret(s.store, s.clusterSecret, node.ID)
			if lerr != nil || !ok || hb.Sig == "" ||
				hb.Timestamp < now-30 || hb.Timestamp > now+30 ||
				!redisacl.VerifyHeartbeatSig(secret, node.Token, hb.Timestamp, hb.Sig) {
				log.Printf("Heartbeat rejected for %s: bad or stale signature", node.Name)
				// Undo the active-mark from step above so a rejected heartbeat
				// cannot keep a node online.
				delete(activeNodeTokens, hb.ID)
				continue
			}
			// Node exists -> Status Update + refresh last_seen_at
			s.store.SetNodeLastSeen(node.ID)
			if node.Status != "online" {
				log.Printf("Node is back online: %s", node.Name)
				s.store.SetNodeStatus(node.ID, "online")
				s.publishServersChanged(ctx)
			}

			// The node's environment is the source of truth for tags and region;
			// the panel only displays them.
			//
			// It used to be the other way round after a single visit to the
			// Configure dialog: that set configured=TRUE, and from then on the
			// env was ignored forever with nothing on the node able to correct
			// it. A Swarm stack sets NODE_TAGS and NODE_REGION once, in one
			// file, for every node it starts - having to repeat that per node in
			// the panel, and having the file silently stop mattering afterwards,
			// is the wrong way round.
			//
			// NAME stays gated, which is not an oversight. The heartbeat's name
			// field carries the node's Core-MINTED identity, not anything from
			// its env, so letting it win would rename an adopted node to a uuid.
			// Nothing sets configured=TRUE any more, so this now only protects
			// rows that were renamed while the Configure dialog still existed.
			if !node.Configured && hb.Name != "" && node.Name != hb.Name {
				log.Printf("Node Name updated: %s → %s", node.Name, hb.Name)
				s.store.SetNodeName(node.ID, hb.Name)
			}

			s.syncNodeMetadataFromHeartbeat(node, hb)

			if hb.IP != "auto" && hb.IP != "" && node.Address != hb.IP {
				log.Printf("Node IP updated for %s: %s -> %s", node.Name, node.Address, hb.IP)
				s.store.SetNodeAddress(node.ID, hb.IP)
			}

			if hb.IPs.Public != "" || len(hb.IPs.Private) > 0 {
				if hb.IPs.Public != node.PublicIP || !slicesEqual(hb.IPs.Private, node.PrivateIPs) {
					s.store.SetNodeIPs(node.ID, hb.IPs.Public, hb.IPs.Private)
				}
			}

			// Cache physical capacity for the scheduler. Older nodes may
			// not yet report total_cpu — we still update RAM in that case.
			totalRAMMB := int64(hb.RAMTotal / (1024 * 1024))
			if totalRAMMB != node.TotalRAMMB || (hb.TotalCPU > 0 && hb.TotalCPU != node.TotalCPU) {
				cpu := hb.TotalCPU
				if cpu == 0 {
					cpu = node.TotalCPU // keep last known
				}
				s.store.UpdateNodeCapacity(node.ID, cpu, totalRAMMB)
			}

			// Hardware-change guard: if the node's host CPU layout changed since we
			// last saw it (e.g. a CPU swap, detected on the node's restart re-scan),
			// reset every CPU pinning on this node so no server / node cpuset
			// references cores that no longer exist.
			s.checkCPUTopologyChange(ctx, node)
		}
	}

	// 4. Offline Check
	//
	// A fleet that could not be read is not an empty fleet. Skipping the sweep
	// is already the safe outcome (nothing gets marked offline on a bad read),
	// but it was indistinguishable from a healthy round in which every node
	// answered - so an operator watching nodes flip had no way to tell the two
	// apart. Say which one happened.
	// Same short-TTL reasoning as the baseline above: a node that stopped
	// reporting must stop claiming an update is pending for it, rather than leave
	// the panel offering a button for a node that is not there.
	if b, err := json.Marshal(linkStates); err == nil {
		s.redis.Set(ctx, NodeLinkStateKey, b, 5*time.Minute)
	}

	dbNodes, err := s.store.ListNodes()
	if err != nil {
		log.Printf("Discovery: offline check skipped, could not list nodes: %v", err)
		return
	}
	for _, dbNode := range dbNodes {
		if _, isActive := activeNodeTokens[dbNode.Token]; !isActive {
			if dbNode.Status == "online" {
				log.Printf("Node went offline: %s", dbNode.Name)
				s.store.SetNodeStatus(dbNode.ID, "offline")
				s.publishServersChanged(ctx)
			}
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
