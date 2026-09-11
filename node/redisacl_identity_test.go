package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Adopting a server-assigned identity writes the package-level nodeID, and by
// the time the ACL watchdog runs, that string is being read by the heartbeat,
// the SFTP listener, the beam server, the stats collector and the gRPC mesh.
// It is also the one thing ensureNodeSecret's hard guard exists to stop a
// paired node doing silently: a new identity orphans the old node row and its
// three scoped Redis ACL users, and every server assigned to the old id lands
// on a node that no longer exists.
func TestIdentityChange(t *testing.T) {
	cases := []struct {
		name                        string
		assigned, current           string
		allow, wantAdopt, wantError bool
	}{
		{name: "first pairing at startup", assigned: "node-a1b2", current: "", allow: true, wantAdopt: true},
		{name: "replacement at startup is loud but allowed", assigned: "node-new", current: "node-old", allow: true, wantAdopt: true},
		{name: "same id is nothing to do", assigned: "node-a1b2", current: "node-a1b2", allow: true},
		{name: "same id, not allowed, still nothing to do", assigned: "node-a1b2", current: "node-a1b2"},
		{name: "no id returned", assigned: "", current: "node-a1b2", allow: true},
		{name: "empty id, not allowed", assigned: "", current: "node-a1b2"},
		// The one that matters: a background re-bootstrap must not re-pair.
		{name: "replacement from the watchdog is refused", assigned: "node-new", current: "node-old", wantError: true},
		{name: "first pairing outside startup is refused too", assigned: "node-new", current: "", wantError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			adopt, err := identityChange(c.assigned, c.current, c.allow)
			if (err != nil) != c.wantError {
				t.Fatalf("err = %v, wantError = %v", err, c.wantError)
			}
			if adopt != c.wantAdopt {
				t.Fatalf("adopt = %v, want %v", adopt, c.wantAdopt)
			}
			// Naming a recovery route at all is the point; it is the panel now
			// rather than an environment variable on the machine.
			if c.wantError && !strings.Contains(err.Error(), "Settings -> Nodes") {
				t.Errorf("the refusal does not say how to recover: %v", err)
			}
		})
	}
}

// identityChange only helps if the callers that run AFTER startup keep passing
// false, and that is a property of the call sites, not of the function. A
// behavioural test cannot see it: the watchdog needs a live Core to reach the
// decision at all.
func TestOnlyEnsureNodeSecretMayChangeIdentity(t *testing.T) {
	var callers []string
	for _, f := range []string{"main.go", "redisacl_bootstrap.go", "grpc_mesh.go", "redis_addr.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(src)
		for _, m := range regexp.MustCompile(`bootstrapSecretViaGRPC\(ctx,\s*(true|false)\)`).FindAllStringSubmatch(body, -1) {
			if m[1] == "true" {
				callers = append(callers, f)
			}
		}
	}
	if len(callers) != 1 || callers[0] != "redisacl_bootstrap.go" {
		t.Fatalf("bootstrapSecretViaGRPC is called with allowIdentityChange=true from %v; "+
			"only ensureNodeSecret (redisacl_bootstrap.go) may, because every other caller runs "+
			"while the rest of the node is already reading nodeID", callers)
	}
}
