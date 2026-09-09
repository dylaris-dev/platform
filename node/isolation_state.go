package main

import (
	"fmt"
	"sync"
)

// What this node can tell an operator about per-tenant network isolation.
//
// It could tell them one thing, once, into stdout at boot: "Tenant network
// isolation ENABLED/DISABLED". That is the wrong place twice over. A node's
// stdout dies with its container, so the answer is gone after the next deploy;
// and the question is asked in the panel, in front of the node list, by
// somebody who is not going to go and read a container log to find out whether
// the isolation they configured is actually in force.
//
// The worse half is the state that is not knowable at boot at all. Isolation
// can be ON and a server still end up on the shared network: the pool runs out,
// the /24 ceiling is reached, the allocator file will not parse, Docker refuses
// the endpoint. Every one of those falls back so the server still starts -
// deliberately, and that decision stands - but it means "isolation is enabled"
// and "this server is isolated" are different facts, and only the first was
// ever reported.

// isolationState tracks the fallbacks since this node started. Written from
// container creation, read from the heartbeat, so it is guarded.
type isolationState struct {
	mu     sync.Mutex
	count  int
	reason string
}

// recordFallback notes one server that could not be given its tenant network.
//
// The LAST reason is kept rather than the first: the first is what an operator
// already reacted to, and the current one is what they need next. The count is
// what makes a single transient failure distinguishable from a node that has
// stopped isolating anything.
func (s *isolationState) recordFallback(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	s.reason = err.Error()
}

func (s *isolationState) snapshot() (count int, reason string) {
	if s == nil {
		return 0, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count, s.reason
}

// isolationOffReason says WHY this node is not isolating.
//
// Three unrelated causes ended in one sentence, and it named SIDECAR_REDIS_ADDR
// for all of them - so two thirds of the nodes reporting "off" were pointing
// their operator at a variable that is either irrelevant or one they must NOT
// set. A host-net node cannot join a per-tenant bridge whatever that variable
// says, and a warp-proxy node has no single Redis address to hand a container
// by construction.
//
// The order is the code's own precedence (main.go): host-net wins, because a
// perfect SIDECAR_REDIS_ADDR would not help there.
//
// The env var is named, its value is not. The name is what an operator edits;
// the value is infrastructure detail that belongs in the node's own log, and
// this string is rendered in a browser.
func isolationOffReason(selfHostNet, viaWarpProxy bool) string {
	switch {
	case selfHostNet:
		return "Off: this node runs on the host network, where it cannot join a per-tenant bridge network, " +
			"so its servers stay on the shared one."
	case viaWarpProxy:
		return "Off: this node reaches Redis through the local warp proxy, whose address is resolved per " +
			"network - there is no single address an isolated server could be given."
	default:
		return "Off: SIDECAR_REDIS_ADDR names a host that only resolves on the shared Docker network, " +
			"so servers stay on it - an isolated server could not reach Redis."
	}
}

// isolationNotice is the one sentence the panel shows beside a node, or "" when
// there is nothing to say.
//
// Empty means "isolation is on and every server got its own network", which is
// the only state that needs no words. The two that do are not the same problem
// and must not read alike: isolation being OFF is a configuration an operator
// chose (or inherited) and can change, while a FALLBACK is isolation failing
// while switched on, which is the one that will not fix itself.
func isolationNotice(enabled bool, offReason string, fallbacks int, lastReason string) string {
	if !enabled {
		return offReason
	}
	if fallbacks == 0 {
		return ""
	}
	servers := "servers"
	if fallbacks == 1 {
		servers = "server"
	}
	return fmt.Sprintf("On, but %d %s went onto the shared network since this node started. Last reason: %s",
		fallbacks, servers, lastReason)
}

// isolationReport is what the heartbeat says about isolation: whether this node
// is isolating, and the sentence that goes with it.
//
// It reads the MANAGER. tenantIsolationEnabled - the variable this used to send
// - answers a different question: whether SIDECAR_REDIS_ADDR permits isolation.
// A host-net node with a perfect address has that variable true and no manager
// at all, so every server on it is on the shared network while the panel showed
// a green "Isolated". The permission and the fact are two records that disagree
// on exactly the nodes where it matters.
//
// A function rather than four lines inside sendHeartbeat because that is the
// difference between a claim that can be tested and one that cannot.
func isolationReport(dm *DockerManager, viaWarpProxy bool) (isolating bool, notice string) {
	if dm == nil {
		return false, ""
	}
	isolating = dm.tenant != nil
	fallbacks, lastReason := dm.isolation.snapshot()
	return isolating, isolationNotice(isolating, isolationOffReason(dm.selfHostNet, viaWarpProxy), fallbacks, lastReason)
}
