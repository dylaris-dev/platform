package services

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"dylaris-core/models"

	"dylaris-core/pkg/leader"

	"github.com/redis/go-redis/v9"
)

// Telling each node which of its servers may be reached by which other server.
//
// The node enforces a default-deny ingress rule per game server: the node and
// the Link always, and beyond that only what this publishes. Everything else is
// refused. See platform/node/netpolicy.go for where the rules actually live and
// why they are written into the container's own network namespace.
//
// WHAT IS ALLOWED, and why the list is this short. A Bungee/Velocity proxy
// dials each backend server; backends never dial each other. All cross-server
// traffic in such a network goes THROUGH the proxy on the bungeecord:main
// plugin channel, so allowing backends to reach one another would open exactly
// the traffic this exists to close, for a case upstream already routes past it.
// Checked against PaperMC's own documentation on 2026-09-10, not assumed.
//
// A proxy therefore appears in its backends' allow lists, and nothing else does
// until somebody adds it by hand.

const netPolicyInterval = 30 * time.Second

// netPolicyStore is the narrow slice this needs, kept small so a test does not
// have to build a whole store.
type netPolicyStore interface {
	ListNodes() ([]models.Node, error)
	ListServers(filterByUser string) ([]models.Server, error)
}

type NetPolicyPublisher struct {
	store  netPolicyStore
	redis  *redis.Client
	leader leader.Election
}

func NewNetPolicyPublisher(s netPolicyStore, r *redis.Client) *NetPolicyPublisher {
	return &NetPolicyPublisher{store: s, redis: r}
}

func (p *NetPolicyPublisher) SetLeader(l leader.Election) { p.leader = l }

// NetPolicyKey must stay byte-identical to netPolicyKey in the node. It sits
// under the prefix the node's own Redis ACL already grants, so neither side
// needed a new permission - and a key outside it would be NOPERM, which reads
// to the node exactly like an empty policy.
func NetPolicyKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":netpolicy"
}

// BuildNodePolicies maps each node id to (server UUID -> allowed peer UUIDs).
//
// Pure, and separate from the writing, because this is the part worth testing:
// getting it wrong either opens a server to a peer nobody linked or cuts a
// proxy off from its backends while players are on it.
//
// The direction matters and is easy to get backwards. The PROXY dials the
// BACKEND, so the backend's ingress list names the proxy. The proxy's own list
// stays empty: players reach it through the Link, which every server accepts
// unconditionally anyway.
func BuildNodePolicies(servers []models.Server) map[int]map[string][]string {
	uuidByID := make(map[int]string, len(servers))
	for _, s := range servers {
		uuidByID[s.ID] = s.UUID
	}

	out := make(map[int]map[string][]string)
	for _, s := range servers {
		if _, ok := out[s.NodeID]; !ok {
			out[s.NodeID] = make(map[string][]string)
		}
		// Every server gets an entry, including one with no peers at all. An
		// absent entry and an empty one mean the same thing to the node, but a
		// present one says "this server was considered", which is what makes a
		// missing server visible rather than silently unpoliced.
		allow := out[s.NodeID][s.UUID]
		if s.ProxyID != nil {
			if proxyUUID, ok := uuidByID[*s.ProxyID]; ok && proxyUUID != "" && proxyUUID != s.UUID {
				allow = append(allow, proxyUUID)
			}
			// A proxy_id pointing at a server that no longer exists contributes
			// nothing rather than an empty string: an empty entry would be
			// rendered as no rule, which is the same outcome, but a "" in the
			// list would travel to the node and be dropped there instead - one
			// more place to have to know about it.
		}
		out[s.NodeID][s.UUID] = allow
	}
	return out
}

func (p *NetPolicyPublisher) Start(ctx context.Context) {
	log.Printf("Network policy publisher started (interval: %s)", netPolicyInterval)
	ticker := time.NewTicker(netPolicyInterval)
	go func() {
		defer ticker.Stop()
		p.RunOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.RunOnce(ctx)
			}
		}
	}()
}

// RunOnce recomputes and publishes every node's policy. Exported so a handler
// can force a pass the moment a proxy link changes, instead of leaving the
// operator to wait out the ticker.
func (p *NetPolicyPublisher) RunOnce(ctx context.Context) {
	if p.redis == nil || p.store == nil {
		return
	}
	// Leader-gated like the other publishers: N replicas writing the same key
	// is N times the work and a race for no gain.
	if p.leader != nil && !p.leader.IsLeader() {
		return
	}

	nodes, err := p.store.ListNodes()
	if err != nil {
		logErrf("netpolicy", "cannot list nodes: %v", err)
		return
	}
	servers, err := p.store.ListServers("")
	if err != nil {
		logErrf("netpolicy", "cannot list servers: %v", err)
		return
	}

	byNode := BuildNodePolicies(servers)
	for _, n := range nodes {
		// A customer's own machine is left alone, deliberately: their servers
		// may talk to each other, and the node there does not enforce anything.
		// Publishing for it would be writing a key nobody reads.
		if n.Kind() == models.NodeKindBYON {
			continue
		}
		policy := byNode[n.ID]
		if policy == nil {
			policy = map[string][]string{}
		}
		raw, merr := json.Marshal(policy)
		if merr != nil {
			logErrf("netpolicy", "cannot encode policy for node %d: %v", n.ID, merr)
			continue
		}
		// No TTL. An expiring key would read to the node as "never published",
		// which switches enforcement OFF - so a Core that paused for a minute
		// would silently unpolice the whole fleet.
		if serr := p.redis.Set(ctx, NetPolicyKey(n.Token), raw, 0).Err(); serr != nil {
			logErrf("netpolicy", "cannot publish policy for node %d: %v", n.ID, serr)
		}
	}
}
