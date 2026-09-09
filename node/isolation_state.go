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

// isolationNotice is the one sentence the panel shows beside a node, or "" when
// there is nothing to say.
//
// Empty means "isolation is on and every server got its own network", which is
// the only state that needs no words. The two that do are not the same problem
// and must not read alike: isolation being OFF is a configuration an operator
// chose (or inherited) and can change, while a FALLBACK is isolation failing
// while switched on, which is the one that will not fix itself.
//
// The env var is named, its value is not. The name is what an operator edits;
// the value is infrastructure detail that belongs in the node's own log, and
// this string is rendered in a browser.
func isolationNotice(enabled bool, fallbacks int, lastReason string) string {
	if !enabled {
		return "Off: SIDECAR_REDIS_ADDR names a host that only resolves on the shared Docker network, " +
			"so servers stay on it - an isolated server could not reach Redis."
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
