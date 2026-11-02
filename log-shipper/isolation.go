package main

import (
	"os"
	"strings"
	"time"
)

// What the server process may see of this one.
//
// The shipper's Redis credential arrives in its environment, and the server
// process runs plugins, mods and possibly a server.jar the tenant uploaded. It
// used to inherit that environment whole, so one System.getenv("REDIS_PASS")
// handed tenant code a login to the SHARED Redis, where a few APPENDs of large
// values could fill it and stop every node, console and route on the platform.
//
// Two things close it, and both are needed. The child gets an environment with
// every REDIS_* variable removed (childEnv). And this process marks itself
// non-dumpable (hideFromSameUID), because the child runs as the same uid and
// could otherwise read the original environment straight out of
// /proc/1/environ - stripping our copy changes nothing there.

// childEnv is env without the shipper's own configuration, plus TERM.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "REDIS_") || k == netGateEnv {
			continue
		}
		out = append(out, e)
	}
	return append(out, "TERM=xterm-256color")
}

// netGateEnv names the file the node creates once this container's network
// rules are in place. Set by the node only where it applies rules; a container
// an older node created has no such variable and starts at once, as before.
//
// The file lives in /data, the server directory, which is root-owned: the
// tenant cannot create it, and no tenant code runs before the server process
// does, so its presence is the node's word.
const netGateEnv = "DYLARIS_NET_GATE"

// netGateWait bounds the wait. Past it the server starts anyway: a node that
// cannot apply rules leaves its servers reachable rather than down (the
// policy's fail-open decision), and the cloud metadata address is dropped on
// the host independently of this.
const netGateWait = 30 * time.Second

// waitForNetGate polls for path until it exists or wait has passed, and
// reports whether it appeared.
func waitForNetGate(path string, wait, every time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(every)
	}
}
