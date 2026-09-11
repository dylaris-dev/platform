package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/redis/go-redis/v9"
)

// Keeping every local game server on the rules it is supposed to have.
//
// A reconciler rather than a hook on the create path, for one measured reason:
// a container restart gives the container a NEW network namespace and the
// rules are simply GONE - after `docker restart` the namespace held only
// Docker's own `table ip nat`. Anything that applied rules once would be
// correct exactly until the first crash-restart, which on a game server is not
// a rare event.
//
// The window this leaves is not a real one. A JVM takes seconds to bind 25565
// and the loop runs every 15s, so by the time a server accepts its first
// packet the rules are on.

// netPolicyKey is where Core publishes this node's policy: serverUUID -> the
// server UUIDs allowed to reach it.
//
// Under dylaris:node:<token>: on purpose - that prefix is already the node's
// own ACL grant (services/redisacl BuildNodeACLRules), so this needed no new
// permission on either side. A new top-level key would have been NOPERM and,
// worse, would have looked like an empty policy rather than an error.
func netPolicyKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":netpolicy"
}

// policySource holds the last policy Core actually published.
//
// It exists because of the direction failures have to fall. A game server's
// proxy reaches it through an ALLOW rule, so an empty policy is not the safe
// answer - it is the answer that cuts a working network's proxy off from its
// backends while players are on it. "We could not read the policy" must
// therefore never be rendered as "nothing is allowed".
//
// Never published: enforce nothing at all, and say so. An old Core that does
// not publish must not be able to break a proxy link it knows nothing about.
// Published and then unreadable: keep the last good one, because a Redis
// hiccup is not a policy change. Published empty: that IS a policy, and it
// means node and Link only.
type policySource struct {
	mu     sync.Mutex
	policy map[string][]string
	seen   bool
}

func (p *policySource) set(m map[string][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m == nil {
		m = map[string][]string{}
	}
	p.policy, p.seen = m, true
}

func (p *policySource) get() (map[string][]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy, p.seen
}

// loadNetPolicy reads the published policy. The second return says whether Core
// has published one at all; redis.Nil is that "no", not an empty policy.
func loadNetPolicy(ctx context.Context, rdb *redis.Client, nodeToken string) (map[string][]string, bool, error) {
	if rdb == nil || nodeToken == "" {
		return nil, false, nil
	}
	raw, err := rdb.Get(ctx, netPolicyKey(nodeToken)).Result()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out map[string][]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, false, err
	}
	if out == nil {
		out = map[string][]string{}
	}
	return out, true, nil
}

// infraPeers are the addresses every server accepts unconditionally: the node
// itself, plus every running Link container on this host.
//
// They are never subject to a rule, and that is deliberate rather than an
// oversight: a rule that can remove the node or the Link is a rule that can
// brick a server. The node is how it is managed (RCON, stats, the tab proxy);
// the Link is the ONLY way in for a player, because in gateway routing an MC
// container binds no host port at all.
//
// The Link is found by isLinkContainer (image, with the legacy fixed name as
// a fallback), never by the one fixed name dylaris_link: once the Link runs
// as a Swarm stack service its task container is named
// <stack>_<svc>.<slot>.<taskid>, generated fresh by Swarm on every deploy, so
// a name-based lookup would return nothing here and every server would start
// refusing the Link while looking healthy. More than one Link may legitimately
// be running - a start-first stack update keeps the old one up beside the new
// one for a moment - and every one of them is allowed.
//
// Resolved on every call, never cached: Docker hands a container a new
// address whenever it is recreated, so a stored address can later belong to
// somebody else's server.
func (dm *DockerManager) infraPeers() []string {
	var out []string
	if dm.selfContainer != "" {
		out = append(out, dm.containerAddrs(dm.selfContainer)...)
	}
	out = append(out, dm.linkAddrs()...)
	return out
}

// containerAddrs returns every network address a container holds. All of them,
// not just the one on the shared network: a node that is also attached
// elsewhere would otherwise arrive from an address no rule mentions.
func (dm *DockerManager) containerAddrs(name string) []string {
	info, err := dm.cli.ContainerInspect(dm.ctx, name)
	if err != nil || info.NetworkSettings == nil {
		return nil
	}
	return networkAddrs(info.NetworkSettings.Networks)
}

// linkAddrs returns every network address held by every running Link
// container on this host, found via isLinkContainer - see infraPeers for why
// that is by image rather than by a fixed name, and why this is never cached.
func (dm *DockerManager) linkAddrs() []string {
	containers, err := dm.cli.ContainerList(dm.ctx, container.ListOptions{})
	if err != nil {
		// Matches ListRunningMCContainers' own list-error handling one pass
		// down in reconcileNetPolicy: log it plainly, no rate limiter - list
		// errors are rare enough that this does not spam.
		log.Printf("netpolicy: cannot list containers to find the Link: %v", err)
		return nil
	}
	return linkContainerAddrs(containers)
}

// linkContainerAddrs picks the Link containers out of a ContainerList result
// and returns every address they hold. Pure, so the "which containers count"
// question is tested without a Docker daemon.
func linkContainerAddrs(containers []container.Summary) []string {
	var out []string
	for _, c := range containers {
		if !isLinkContainer(c.Names, c.Image) || c.NetworkSettings == nil {
			continue
		}
		out = append(out, networkAddrs(c.NetworkSettings.Networks)...)
	}
	return out
}

// networkAddrs turns a container's per-network endpoints into every address it
// holds: all networks, not just one, and both IPv4 and global IPv6. Shared by
// containerAddrs (one container, via ContainerInspect) and linkContainerAddrs
// (many, via ContainerList) - both endpoint types carry the same field shape.
func networkAddrs(networks map[string]*network.EndpointSettings) []string {
	var out []string
	for _, ep := range networks {
		if ep == nil {
			continue
		}
		if ep.IPAddress != "" {
			out = append(out, ep.IPAddress)
		}
		if ep.GlobalIPv6Address != "" {
			out = append(out, ep.GlobalIPv6Address)
		}
	}
	return out
}

// peerAddrs turns the server UUIDs Core allowed into addresses.
//
// Resolved every pass rather than stored. Docker's IPAM hands a container a new
// address whenever it is recreated, so an address written into a rule is stale
// the moment the peer restarts - and a stale allow rule is worse than a missing
// one, because the address may by then belong to somebody else's server.
//
// A peer on THIS host is read from Docker directly; one on another host is
// resolved by name, which works because every server sits on the same
// swarm-wide overlay and that is exactly how a route already finds mc_<uuid>.
func (dm *DockerManager) peerAddrs(uuids []string) []string {
	var out []string
	for _, u := range uuids {
		name := "mc_" + u
		if addrs := dm.containerAddrs(name); len(addrs) > 0 {
			out = append(out, addrs...)
			continue
		}
		ips, err := net.LookupIP(name)
		if err != nil {
			// Not worth failing the server for: a proxy that is not running has
			// no address to allow, and the rule appears on the next pass once it
			// does.
			continue
		}
		for _, ip := range ips {
			out = append(out, ip.String())
		}
	}
	return out
}

// reconcileNetPolicy brings every local game server to its wanted ruleset once.
func (dm *DockerManager) reconcileNetPolicy(ctx context.Context, rdb *redis.Client, nodeToken string) {
	fresh, published, err := loadNetPolicy(ctx, rdb, nodeToken)
	switch {
	case err != nil:
		// A read failure is not a policy change. Keep the last good one; if
		// there has never been one, enforce nothing rather than enforce a
		// narrower set than Core intends.
		log.Printf("netpolicy: cannot read the published policy: %v", err)
	case published:
		dm.netPolicySrc.set(fresh)
	}

	policy, haveSource := dm.netPolicySrc.get()
	if !haveSource {
		// Deliberate, and the reason is the direction failures fall. Applying
		// node+Link only would cut a linked proxy off from its backends - on a
		// running network, with players on it - because Core happened not to
		// have published yet. Nothing is enforced until Core has spoken once.
		dm.netPolicy.recordUnpublished()
		return
	}

	containers, lerr := dm.ListRunningMCContainers()
	if lerr != nil {
		log.Printf("netpolicy: cannot list containers: %v", lerr)
		return
	}
	if len(containers) == 0 {
		dm.netPolicy.recordApplied(0)
		return
	}

	infra := dm.infraPeers()
	if len(infra) == 0 {
		// Without the node's and the Link's own addresses every rule set would
		// lock the server away from its own management and from every player.
		// That is the one case where applying is worse than not applying.
		log.Printf("netpolicy: neither this node nor the Link has a resolvable address; leaving every server OPEN")
		dm.netPolicy.recordFailure("-", errNoInfraAddrs)
		return
	}

	applied := 0
	for _, c := range containers {
		allow := append([]string{}, infra...)
		allow = append(allow, dm.peerAddrs(policy[c.UUID])...)

		applyCtx, cancel := context.WithTimeout(ctx, netPolicyApplyTimeout)
		aerr := dm.applyNetPolicy(applyCtx, c.ContainerName, allow)
		cancel()
		if aerr != nil {
			dm.netPolicy.recordFailure(c.UUID, aerr)
			log.Printf("netpolicy: %s left OPEN: %v", c.ContainerName, aerr)
			continue
		}
		applied++
	}
	dm.netPolicy.recordApplied(applied)
}

// startNetPolicyReconciler runs the loop until the context ends.
//
// Not started at all on an external or BYON node. That machine belongs to the
// customer: separating their own servers from each other protects nobody, and
// the node there would be reaching into containers on hardware that is not
// ours. Decided 2026-09-10.
func startNetPolicyReconciler(ctx context.Context, dm *DockerManager, rdb *redis.Client, nodeToken string) {
	if dm == nil || dm.netPolicyImage == "" {
		log.Printf("netpolicy: no helper image resolved; per-server rules are NOT being applied")
		return
	}
	log.Printf("netpolicy: enforcing per-server ingress rules every %s (helper image %s)",
		netPolicyInterval, dm.netPolicyImage)
	go func() {
		t := time.NewTicker(netPolicyInterval)
		defer t.Stop()
		dm.reconcileNetPolicy(ctx, rdb, nodeToken)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				dm.reconcileNetPolicy(ctx, rdb, nodeToken)
			}
		}
	}()
}
