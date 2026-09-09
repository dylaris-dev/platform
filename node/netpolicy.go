package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// Per-server network policy: a game server accepts connections from the node,
// from the Link, and from nothing else unless something says so.
//
// WHY THIS SHAPE, and not a network per tenant. A proxy and the servers behind
// it belong to the SAME owner, so a per-owner network leaves exactly the
// traffic this is about completely unrestricted. It also answers a question
// nobody asked - two different owners reaching each other - which Docker's own
// inter-network isolation already answered.
//
// WHERE IT IS ENFORCED. In the container's OWN network namespace. Two
// containers on one Docker overlay are bridged inside that overlay's namespace
// and never cross the host's FORWARD chain, so DOCKER-USER cannot see that
// traffic at all. The container's netns is the one place that behaves the same
// on a bridge and on an overlay. Measured on the live overlay 2026-09-10:
// before the rule the peer REACHED port 25565, after it the peer was REFUSED
// and the Link still REACHED it.
//
// HOW THE RULES GET THERE, and this is the part worth not changing. The node
// does not need CAP_NET_ADMIN, host /proc or nsenter. It starts a throwaway
// container joined to the target's namespace
// (`--network container:<target> --cap-add NET_ADMIN`) through the Docker
// socket it already holds; that container writes the ruleset and exits. The
// privilege is borrowed for a second by something that goes away, instead of
// being granted permanently to the agent that runs on every host.
//
// FAIL OPEN, LOUDLY. Owner's decision. With fail-closed, a fault here cuts off
// the node, the Link and the proxy as well, so the server is unreachable by
// players AND unmanageable by us - worse than the leak it would prevent. A
// server whose rules could not be applied therefore keeps running and reachable,
// and the failure is counted and reported in the heartbeat (see netPolicyState).
// Silence is what would make fail-open dangerous.

const (
	// netPolicyTable is ours alone. Docker owns "ip nat" in the same namespace
	// and must not be touched.
	netPolicyTable = "dylaris"
	// netPolicyInterval is how often every local MC container is brought back to
	// its wanted ruleset. It is also the repair loop: a container restart gives
	// it a NEW network namespace and the rules are simply gone (measured), so
	// re-application is mandatory rather than an optimisation.
	netPolicyInterval = 15 * time.Second
	// netPolicyApplyTimeout bounds one helper run. It pulls nothing - the image
	// is the node's own, already on the host.
	netPolicyApplyTimeout = 30 * time.Second
)

// netPolicyRuleset renders the nft script for one server.
//
// The allowlist is INGRESS only. Egress is deliberately unfiltered: a server
// dialling a peer is refused at the peer, so filtering both directions would
// add a second place to get it wrong and would break Redis, the log-shipper and
// outbound plugin traffic for nothing.
//
// `table inet dylaris` before `delete table inet dylaris` is not a typo: the
// first line creates it when absent, so the delete cannot fail on a container
// that has never been touched. That makes the whole script idempotent, which is
// what lets the reconciler run it unconditionally.
func netPolicyRuleset(allow []string) string {
	var b strings.Builder
	b.WriteString("table inet " + netPolicyTable + "\n")
	b.WriteString("delete table inet " + netPolicyTable + "\n")
	b.WriteString("table inet " + netPolicyTable + " {\n")
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority 0; policy drop;\n")
	// Replies to connections the server itself opened. Without this the server
	// could not talk to Redis or fetch anything.
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iif lo accept\n")
	// ICMP stays: path-MTU discovery is carried by it, and a silently black-holed
	// PMTU is the kind of fault that looks like a random disconnect.
	b.WriteString("    ip protocol icmp accept\n")
	b.WriteString("    ip6 nexthdr icmpv6 accept\n")
	for _, a := range dedupeSorted(allow) {
		if ip := net.ParseIP(a); ip == nil {
			continue // never interpolate anything unparsed into a ruleset
		} else if ip.To4() != nil {
			b.WriteString("    ip saddr " + a + " accept\n")
		} else {
			b.WriteString("    ip6 saddr " + a + " accept\n")
		}
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// netPolicyState is what this node can tell an operator about the policy.
//
// Same shape and the same reason as isolationState: a failure that only ever
// reached a log line is a failure nobody sees, because a node's stdout dies
// with its container. Fail-open is only acceptable while the failing is loud.
type netPolicyState struct {
	mu sync.Mutex
	// unpublished is the state before Core has ever published a policy for this
	// node. It is NOT a failure and must not read like one: nothing is enforced
	// yet, deliberately, because narrowing on a guess would cut a linked proxy
	// off from its backends. See policySource.
	unpublished bool
	failures    int
	reason      string
	applied     int
}

// errNoInfraAddrs is the one case where applying is worse than not applying:
// without the node's and the Link's own addresses, every ruleset would lock the
// server away from its own management AND from every player.
var errNoInfraAddrs = errors.New("node and Link have no resolvable address")

func (s *netPolicyState) recordUnpublished() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unpublished, s.applied = true, 0
}

func (s *netPolicyState) recordFailure(uuid string, err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.reason = fmt.Sprintf("%s: %v", shortUUID(uuid), err)
}

func (s *netPolicyState) recordApplied(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied, s.unpublished = n, false
}

func (s *netPolicyState) snapshot() (applied, failures int, reason string, unpublished bool) {
	if s == nil {
		return 0, 0, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied, s.failures, s.reason, s.unpublished
}

// netPolicyNotice is the one sentence the panel shows, or "" when there is
// nothing to say. Enforcement being OFF is a deployment fact, not a fault: an
// external or BYON node runs on hardware that is not ours, where separating a
// customer's own servers from each other buys nobody anything.
func netPolicyNotice(enforced bool, external bool, applied, failures int, lastReason string, unpublished bool) string {
	if !enforced {
		if external {
			return "Off: this is not one of our machines, so its servers are left on the plain Docker network."
		}
		return "Off: no policy is being applied on this node."
	}
	// Waiting, not failing, and the difference is what an operator would do
	// about it. Nothing is enforced until Core has published once, so that a
	// Core which knows nothing about this cannot narrow a working network.
	if unpublished {
		return "Waiting: this node has not been sent a policy yet, so its servers accept any other server until it arrives."
	}
	if failures == 0 {
		return ""
	}
	servers := "servers"
	if failures == 1 {
		servers = "server"
	}
	return fmt.Sprintf("On for %d, but %d %s could not be given their rules since this node started and are reachable from any other server. Last error: %s",
		applied, failures, servers, lastReason)
}

func shortUUID(u string) string {
	if len(u) > 8 {
		return u[:8]
	}
	return u
}

// applyNetPolicy writes the ruleset into one running container's namespace.
//
// The helper is the node's OWN image, which is already on every host that runs
// a node - so this pulls nothing and adds no image to publish. It carries
// nftables for exactly this (see the Dockerfile).
func (dm *DockerManager) applyNetPolicy(ctx context.Context, containerName string, allow []string) error {
	if dm.netPolicyImage == "" {
		return fmt.Errorf("no helper image resolved")
	}
	script := netPolicyRuleset(allow)
	cfg := &container.Config{
		Image: dm.netPolicyImage,
		// The ruleset travels in the command, not on stdin: attaching a stream to
		// a one-shot container is a second failure mode for no gain.
		Cmd:        []string{"sh", "-c", "printf '%s' \"$0\" | nft -f -", script},
		Entrypoint: []string{},
	}
	hc := &container.HostConfig{
		// Joins the TARGET's network namespace. This is the whole mechanism.
		NetworkMode: container.NetworkMode("container:" + containerName),
		CapAdd:      []string{"NET_ADMIN"},
		AutoRemove:  true,
	}
	resp, err := dm.cli.ContainerCreate(ctx, cfg, hc, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return fmt.Errorf("create policy helper: %w", err)
	}
	if err := dm.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start policy helper: %w", err)
	}
	statusCh, errCh := dm.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return fmt.Errorf("wait policy helper: %w", err)
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("policy helper exited %d", st.StatusCode)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// resolveSelfImage reads the image reference this node is running from, so the
// policy helper can be the same one. Asking Docker beats an env var: the value
// is then correct by construction on every deployment, including one where the
// operator pinned a digest.
func (dm *DockerManager) resolveSelfImage(self string) string {
	if self == "" {
		return ""
	}
	info, err := dm.cli.ContainerInspect(dm.ctx, self)
	if err != nil || info.Config == nil {
		return ""
	}
	return info.Config.Image
}
