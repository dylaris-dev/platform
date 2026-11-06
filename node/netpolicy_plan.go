package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/redis/go-redis/v9"
)

// What a reconcile round applies, decided once per round.
//
// Two layers, and they fail in different directions on purpose:
//
//   - EGRESS is applied to every server on every node that runs the reconciler,
//     whether or not Core has published anything: it does not depend on Core's
//     allowlist. On a machine whose servers are separated it is the full chain
//     (host, LAN, platform ports); elsewhere it is the link-local drop alone,
//     so a customer's single-owner machine keeps reaching its own LAN. The
//     link-local drop is in every variant (see writeEgressChain).
//   - INGRESS, the per-server allowlist, separates servers from each other. It
//     needs Core's published policy, and without it nothing is narrowed, for
//     the reason policySource gives.
//
// Which machines separate their servers: every platform node, and an external
// or BYON node once Core says it holds servers of more than one owner
// (netPolicyTenancyKey) or the operator sets NETPOLICY=on. Decided 2026-11-02:
// isolation follows tenancy, not "external". It used to be skipped on every
// external node, and the operator's own home machine hosted four different
// people's servers with each able to reach the others' RCON and the LAN.

// netPolicyTenancyKey is where Core says an external or BYON machine holds
// servers of more than one owner. Under the node's own prefix, which its Redis
// ACL already grants. An older Core never writes it, so a new node on an old
// Core keeps such a machine exactly as it was apart from the link-local drop.
func netPolicyTenancyKey(nodeToken string) string {
	return "dylaris:node:" + nodeToken + ":netpolicy_tenancy"
}

// netPlan is one round's decision.
type netPlan struct {
	separate    bool // write the ingress allowlist
	unpublished bool // this machine separates servers but Core has not spoken yet
	noInfra     bool // it should separate them but cannot find node or Link
	policy      map[string][]string
	// peerCache resolves each peer once per round. On a machine separated by
	// owner every server lists every other server of its owner, so resolving
	// per entry was a Docker inspect per pair, every 15 s.
	peerCache map[string][]string
	infra     []string
	eg        *netEgress // nil: the link-local drop only
}

// sharedTenancy reports whether Core marks this machine as holding servers of
// more than one owner. A read error keeps the last answer: a Redis hiccup is
// not a change of tenants.
func (dm *DockerManager) sharedTenancy(ctx context.Context, rdb *redis.Client, nodeToken string) bool {
	if rdb == nil || nodeToken == "" {
		return dm.tenancyShared.Load()
	}
	v, err := rdb.Get(ctx, netPolicyTenancyKey(nodeToken)).Result()
	switch {
	case err == redis.Nil:
		dm.tenancyShared.Store(false)
	case err != nil:
		log.Printf("netpolicy: cannot read the tenancy flag, keeping the last answer: %v", err)
	default:
		dm.tenancyShared.Store(v == "shared")
	}
	return dm.tenancyShared.Load()
}

// planNetPolicy decides the round.
func (dm *DockerManager) planNetPolicy(ctx context.Context, rdb *redis.Client, nodeToken string) netPlan {
	// The flag before the policy: Core writes them the other way round, so a
	// "shared" flag read here is never paired with an older, narrower list.
	separate := !dm.external || dm.netPolicyForce || dm.sharedTenancy(ctx, rdb, nodeToken)
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

	dm.netPolicySeparates.Store(separate)
	plan := netPlan{policy: policy, peerCache: map[string][]string{}}
	if !separate {
		return plan
	}
	eg, egErr := dm.egressPolicy()
	if egErr != nil {
		log.Printf("netpolicy: only the link-local drop this round, the rest of egress is left OPEN: %v", egErr)
	}
	plan.eg = eg
	if !haveSource {
		// Deliberate, and the reason is the direction failures fall. Applying
		// node+Link only would cut a linked proxy off from its backends - on a
		// running network, with players on it - because Core happened not to
		// have published yet. Egress does not depend on Core and goes on now.
		plan.unpublished = true
		return plan
	}
	plan.infra = dm.infraPeers()
	if len(plan.infra) == 0 {
		// Without the node's and the Link's own addresses every allowlist would
		// lock the server away from its own management and from every player.
		// That is the one case where separating is worse than not.
		log.Printf("netpolicy: neither this node nor the Link has a resolvable address; servers are NOT separated this round")
		plan.noInfra = true
		return plan
	}
	plan.separate = true
	return plan
}

// applyPlanned writes the round's rules into one server.
func (dm *DockerManager) applyPlanned(ctx context.Context, plan netPlan, c MCContainer) error {
	var allow []string
	if plan.separate {
		allow = append(allow, plan.infra...)
		for _, peer := range plan.policy[c.UUID] {
			addrs, ok := plan.peerCache[peer]
			if !ok {
				addrs = dm.peerAddrs([]string{peer})
				plan.peerCache[peer] = addrs
			}
			allow = append(allow, addrs...)
		}
	}
	applyCtx, cancel := context.WithTimeout(ctx, netPolicyApplyTimeout)
	defer cancel()
	if err := dm.applyRuleset(applyCtx, "container:"+c.ContainerName, renderNetPolicy(plan.separate, allow, c.Published, plan.eg)); err != nil {
		return err
	}
	if plan.noInfra {
		return errNoInfraAddrs
	}
	return nil
}

// reconcileNetPolicy brings every local game server to its wanted ruleset once.
func (dm *DockerManager) reconcileNetPolicy(ctx context.Context, rdb *redis.Client, nodeToken string) {
	dm.netPolicyRoundMu.Lock()
	defer dm.netPolicyRoundMu.Unlock()
	dm.ensureHostRules(ctx)
	plan := dm.planNetPolicy(ctx, rdb, nodeToken)

	containers, lerr := dm.ListRunningMCContainers()
	if lerr != nil {
		log.Printf("netpolicy: cannot list containers: %v", lerr)
		return
	}
	applied := 0
	failed := map[string]error{}
	for _, c := range containers {
		if err := dm.applyPlanned(ctx, plan, c); err != nil {
			failed[c.UUID] = err
			log.Printf("netpolicy: %s left OPEN: %v", c.ContainerName, err)
			continue
		}
		applied++
	}
	if plan.unpublished {
		dm.netPolicy.recordUnpublished()
		return
	}
	dm.netPolicy.recordRound(applied, failed)
}

// startNetPolicyReconciler runs the loop until the context ends.
func startNetPolicyReconciler(ctx context.Context, dm *DockerManager, rdb *redis.Client, nodeToken string) {
	if dm == nil || dm.netPolicyImage == "" {
		log.Printf("netpolicy: no helper image resolved; per-server rules are NOT being applied")
		return
	}
	dm.netPolicyRDB, dm.netPolicyToken = rdb, nodeToken
	go dm.watchLinkAddrs(ctx)
	log.Printf("netpolicy: applying per-server rules every %s (helper image %s)",
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
			case <-dm.netPolicyKick:
				dm.reconcileNetPolicy(ctx, rdb, nodeToken)
			}
		}
	}()
}

// kickNetPolicy asks for a round now instead of at the next tick.
// Non-blocking: one pending kick covers any number of starts, and a node that
// runs no reconciler just leaves it pending.
func (dm *DockerManager) kickNetPolicy() {
	select {
	case dm.netPolicyKick <- struct{}{}:
	default:
	}
}

// netGateFile is the marker the log-shipper waits for before it starts the
// server process (DYLARIS_NET_GATE). It sits in the server directory, which is
// root-owned, so the tenant cannot create it; the node removes it before every
// start and writes it once that container's rules are on. That is what closes
// the window the reconciler alone left: a server's code ran unfiltered until
// the next round, long enough to read the cloud metadata service.
const netGateFile = ".dylaris-net-ready"

// netGateEnv is the variable that tells the log-shipper where the marker is.
const netGateEnv = "DYLARIS_NET_GATE"

// netGateContainerEnv is the env to bake into a new game container, or nil on
// a node that applies no rules (then there is nothing to wait for).
func (dm *DockerManager) netGateContainerEnv() []string {
	if dm.netPolicyRDB == nil {
		return nil
	}
	return []string{netGateEnv + "=/data/" + netGateFile}
}

// closeNetGate removes the marker before a start, so the shipper waits.
func (dm *DockerManager) closeNetGate(uuid string) {
	if err := os.Remove(filepath.Join(dm.resolveLocalServerPath(uuid), netGateFile)); err != nil && !os.IsNotExist(err) {
		log.Printf("netpolicy: cannot reset the start gate of %s: %v", uuid, err)
	}
}

// openNetGate applies the rules to a server that just started and then lets
// its server process run. The marker is written whether or not the apply
// worked: a failure is FAIL OPEN and reported like any other (the round
// records it), and holding the server would only delay that by the shipper's
// own timeout. Written on a node that applies nothing, too, so a container
// created while it did never waits for a gate nobody opens.
func (dm *DockerManager) openNetGate(uuid string) {
	if dm.netPolicyRDB != nil {
		plan := dm.planNetPolicy(dm.ctx, dm.netPolicyRDB, dm.netPolicyToken)
		if running, err := dm.ListRunningMCContainers(); err == nil {
			for _, c := range running {
				if c.UUID == uuid {
					if aerr := dm.applyPlanned(dm.ctx, plan, c); aerr != nil {
						log.Printf("netpolicy: %s started OPEN: %v", c.ContainerName, aerr)
					}
				}
			}
		}
		dm.kickNetPolicy()
	}
	// Through the server's Root: under MC_RUN_AS=0 the tenant can swap the
	// marker for a link, and a plain WriteFile truncated its target as root.
	if err := writeServerRootFile(dm.resolveLocalServerPath(uuid), netGateFile, nil); err != nil {
		log.Printf("netpolicy: cannot open the start gate of %s (its server waits %s): %v", uuid, 30*time.Second, err)
	}
}

// sharedNetInfo is the shared network's IPv4 subnet, gateway and driver.
func (dm *DockerManager) sharedNetInfo() (id, subnet, gateway, driver string, err error) {
	id, _, err = dm.ensureGlobalNetwork()
	if err != nil {
		return "", "", "", "", err
	}
	insp, err := dm.cli.NetworkInspect(dm.ctx, id, network.InspectOptions{})
	if err != nil {
		return "", "", "", "", fmt.Errorf("inspect network: %w", err)
	}
	for _, c := range insp.IPAM.Config {
		if _, n, perr := net.ParseCIDR(c.Subnet); perr == nil && n.IP.To4() != nil {
			subnet = n.String()
			if gw := net.ParseIP(c.Gateway); gw != nil && gw.To4() != nil {
				gateway = gw.String()
			}
			break
		}
	}
	if subnet == "" {
		return "", "", "", "", errors.New("the shared network has no IPv4 subnet")
	}
	return id, subnet, gateway, insp.Driver, nil
}

// egressPolicy works out the shared network's subnet and gateway, the Redis
// address a container's log-shipper dials and the game port.
func (dm *DockerManager) egressPolicy() (*netEgress, error) {
	netID, subnet, gateway, _, err := dm.sharedNetInfo()
	if err != nil {
		return nil, err
	}
	addr, err := dm.sidecarRedisAddr(netID, nodeRedis.current())
	if err != nil {
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("redis address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("redis port %q: %w", portStr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, lerr := net.LookupIP(host)
		if lerr != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve redis host %q: %v", host, lerr)
		}
		for _, cand := range ips {
			if cand.To4() != nil {
				ip = cand
				break
			}
		}
		if ip == nil {
			return nil, fmt.Errorf("redis host %q has no IPv4 address", host)
		}
	}
	_, _, _, gamePort, _, _ := getModes()
	return &netEgress{overlay: subnet, redisIP: ip.String(), redisPort: port, gamePort: gamePort, gateway: gateway}, nil
}

// The host-level block of the cloud metadata address.
//
// Independent of everything above: of Core, of the per-server rules and of the
// start gate. It lives in the HOST's netfilter, in a table of its own, so it
// holds while a container's own rules are not there yet or failed, and it does
// not care whether the host's Docker writes iptables through nft or legacy -
// a drop in any table at the forward hook is final. Scoped to traffic from the
// servers' network (and, on a Swarm node, the docker_gwbridge every overlay
// container leaves through), so other containers on a customer's machine that
// rely on their cloud's metadata keep it.
const (
	hostRulesTable    = "dylaris_host"
	hostRulesInterval = 5 * time.Minute
	metadataAddr      = "169.254.169.254"
)

// hostRuleset renders the host table, or "" when nothing valid is known.
func hostRuleset(subnet string, gwbridge bool) string {
	var rules []string
	if _, n, err := net.ParseCIDR(subnet); err == nil && n.IP.To4() != nil {
		rules = append(rules, "    ip saddr "+n.String()+" ip daddr "+metadataAddr+" drop\n")
	}
	if gwbridge {
		rules = append(rules, "    iifname \"docker_gwbridge\" ip daddr "+metadataAddr+" drop\n")
	}
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("table inet " + hostRulesTable + "\n")
	b.WriteString("delete table inet " + hostRulesTable + "\n")
	b.WriteString("table inet " + hostRulesTable + " {\n")
	b.WriteString("  chain forward {\n")
	b.WriteString("    type filter hook forward priority -10; policy accept;\n")
	for _, r := range rules {
		b.WriteString(r)
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// hostRulesState remembers when the host table was last written and why it
// last failed, for the heartbeat.
type hostRulesState struct {
	mu      sync.Mutex
	last    time.Time
	problem string
}

func (h *hostRulesState) due(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last.IsZero() || now.Sub(h.last) >= hostRulesInterval || h.problem != ""
}

func (h *hostRulesState) record(now time.Time, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = now
	h.problem = ""
	if err != nil {
		h.problem = err.Error()
	}
}

// Problem is "" while the host block is in place.
func (h *hostRulesState) Problem() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.problem
}

// ensureHostRules (re)writes the host table every hostRulesInterval, and every
// round while it is failing. Rewritten rather than checked, because a firewall
// reload on the host can remove it and the script is idempotent.
func (dm *DockerManager) ensureHostRules(ctx context.Context) {
	now := time.Now()
	if !dm.hostRules.due(now) {
		return
	}
	_, subnet, _, driver, err := dm.sharedNetInfo()
	if err == nil {
		// Only on the platform's own Swarm nodes: on a customer's machine the
		// gwbridge also carries containers that are none of ours.
		script := hostRuleset(subnet, driver == "overlay" && !dm.external)
		if script == "" {
			err = errors.New("no usable subnet for the servers' network")
		} else {
			applyCtx, cancel := context.WithTimeout(ctx, netPolicyApplyTimeout)
			err = dm.applyRuleset(applyCtx, "host", script)
			cancel()
		}
	}
	if err != nil {
		log.Printf("netpolicy: the host-level block of %s is NOT in place: %v", metadataAddr, err)
	}
	dm.hostRules.record(now, err)
}

// reconcileNetPolicyNow runs one full round synchronously, on a node whose
// reconciler runs. Serialised with the loop's rounds by netPolicyRoundMu so the
// caller knows its round, not a concurrent one, has finished.
func (dm *DockerManager) reconcileNetPolicyNow(ctx context.Context) {
	if dm == nil || dm.netPolicyRDB == nil {
		return
	}
	dm.reconcileNetPolicy(ctx, dm.netPolicyRDB, dm.netPolicyToken)
}

// linkWatchInterval is how often the set of Link addresses is compared. A Link
// that came back with a new address (a crash restart, a recreate by hand, a
// start-first update) is refused by every server until a round names it, so a
// change triggers one at once instead of waiting out netPolicyInterval.
const linkWatchInterval = 2 * time.Second

// linkAddrsKey is the comparable form of linkAddrs.
func linkAddrsKey(addrs []string) string {
	return strings.Join(dedupeSorted(addrs), ",")
}

func (dm *DockerManager) watchLinkAddrs(ctx context.Context) {
	t := time.NewTicker(linkWatchInterval)
	defer t.Stop()
	last := linkAddrsKey(dm.linkAddrs())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if now := linkAddrsKey(dm.linkAddrs()); now != last {
			last = now
			log.Printf("netpolicy: the Link addresses changed (%s); applying rules now", now)
			dm.kickNetPolicy()
		}
	}
}

// removeNetPolicy is NETPOLICY=off actively undoing what an earlier run put in
// place: the per-server tables in every running server, the host table, and
// the start gate of every server directory, so nothing keeps waiting for rules
// that no longer come. A container's rules otherwise lived on until its next
// restart, which made switching the policy off no rollback at all.
func (dm *DockerManager) removeNetPolicy(ctx context.Context, image string) {
	if image == "" {
		log.Printf("netpolicy: off, but no helper image resolved; rules applied earlier stay until each server restarts")
		return
	}
	running, err := dm.ListRunningMCContainers()
	if err != nil {
		log.Printf("netpolicy: off, cannot list servers to remove their rules: %v", err)
	}
	removed := 0
	for _, c := range running {
		actx, cancel := context.WithTimeout(ctx, netPolicyApplyTimeout)
		err := dm.applyRulesetWith(actx, image, "container:"+c.ContainerName, dropTableScript(netPolicyTable))
		cancel()
		dm.openNetGate(c.UUID)
		if err != nil {
			log.Printf("netpolicy: off, cannot remove the rules of %s: %v", c.ContainerName, err)
			continue
		}
		removed++
	}
	actx, cancel := context.WithTimeout(ctx, netPolicyApplyTimeout)
	if err := dm.applyRulesetWith(actx, image, "host", dropTableScript(hostRulesTable)); err != nil {
		log.Printf("netpolicy: off, cannot remove the host table %s: %v", hostRulesTable, err)
	}
	cancel()
	log.Printf("netpolicy: off; removed the rules of %d running server(s) and the host table", removed)
}

// dropTableScript removes an inet table whether or not it exists (the same
// create-then-delete idiom as the rulesets).
func dropTableScript(table string) string {
	return "table inet " + table + "\ndelete table inet " + table + "\n"
}
