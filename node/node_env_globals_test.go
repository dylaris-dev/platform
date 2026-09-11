package main

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// envMap turns a "KEY=VALUE" env slice (as built by buildRedisEnv/buildLinkEnv)
// into a map for easy per-key assertions. Fails the test on any malformed entry.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed env entry %q", kv)
		}
		m[parts[0]] = parts[1]
	}
	return m
}

// These tests read and mutate PACKAGE GLOBALS (nodeID, redisDB, nodeSecret).
// Every subtest saves the old values and restores them via defer/t.Cleanup, and
// none of them call t.Parallel(), per the wave brief - mutating shared package
// state concurrently would corrupt other tests.

func TestBuildRedisEnv(t *testing.T) {
	origNodeID := nodeID
	origRedisDB := redisDB
	origNodeSecret := nodeSecret
	t.Cleanup(func() {
		nodeID = origNodeID
		redisDB = origRedisDB
		nodeSecret = origNodeSecret
	})

	const addr = "10.1.2.3:6379"
	nodeID = "node-test-1"
	redisDB = 2
	nodeSecret = []byte("unit-test-secret-value-buildredisenv")

	t.Run("with sub-server: full 7-entry env, correctly scoped ACL user/pass", func(t *testing.T) {
		got := buildRedisEnv("uuid-abc", "sub1", addr)
		if len(got) != 7 {
			t.Fatalf("got %d env entries, want 7: %v", len(got), got)
		}
		m := envMap(t, got)

		wantUser := aclShipperUsername(nodeID, "uuid-abc")
		wantPass := aclShipperPassword(nodeSecret, nodeID, "uuid-abc")

		if m["REDIS_ADDR"] != addr {
			t.Errorf("REDIS_ADDR = %q, want %q", m["REDIS_ADDR"], addr)
		}
		if m["REDIS_USER"] != wantUser {
			t.Errorf("REDIS_USER = %q, want %q (scoped shipper user, NOT plain node user)", m["REDIS_USER"], wantUser)
		}
		if m["REDIS_PASS"] != wantPass {
			t.Errorf("REDIS_PASS = %q, want %q", m["REDIS_PASS"], wantPass)
		}
		// The node's own DB: SIDECAR_REDIS_DB is gone, and there is one Redis.
		if m["REDIS_DB"] != "2" {
			t.Errorf("REDIS_DB = %q, want %q", m["REDIS_DB"], "2")
		}
		if m["SERVER_UUID"] != "uuid-abc" {
			t.Errorf("SERVER_UUID = %q, want %q", m["SERVER_UUID"], "uuid-abc")
		}
		if m["TERM"] != "xterm-256color" {
			t.Errorf("TERM = %q, want xterm-256color", m["TERM"])
		}
		if m["SUB_SERVER"] != "sub1" {
			t.Errorf("SUB_SERVER = %q, want %q", m["SUB_SERVER"], "sub1")
		}
	})

	t.Run("without sub-server: SUB_SERVER omitted (6 entries)", func(t *testing.T) {
		got := buildRedisEnv("uuid-abc", "", addr)
		if len(got) != 6 {
			t.Fatalf("got %d env entries, want 6: %v", len(got), got)
		}
		m := envMap(t, got)
		if _, ok := m["SUB_SERVER"]; ok {
			t.Errorf("SUB_SERVER should be absent when subServer is empty, got %v", got)
		}
	})
}

func TestBuildLinkEnv(t *testing.T) {
	origRedisDB := redisDB
	origNodeSecret := nodeSecret
	origClusterSecret := clusterSecret
	origNodeExternal := nodeExternal
	t.Cleanup(func() {
		redisDB = origRedisDB
		nodeSecret = origNodeSecret
		clusterSecret = origClusterSecret
		nodeExternal = origNodeExternal
	})

	const addr = "10.9.9.9:6379"
	redisDB = 5
	nodeSecret = []byte("unit-test-secret-value-buildlinkenv")
	// What makes this node in-cluster, and the only thing that does. Without it
	// the node is BYON and the link belongs on the public address.
	clusterSecret = "unit-test-cluster-secret"
	nodeExternal = false

	// buildLinkEnv takes nodeID as a PARAMETER (it shadows the package
	// global inside the function body), so the package-global nodeID is
	// deliberately left untouched by this test.
	testNodeID := "nodeXYZ"
	got := buildLinkEnv(testNodeID, "tunnel-secret-1", "discovery-proof-1", addr)

	if len(got) != 8 {
		t.Fatalf("got %d env entries, want 8: %v", len(got), got)
	}
	m := envMap(t, got)

	wantUser := aclLinkUsername(testNodeID)
	wantPass := aclLinkPassword(nodeSecret, testNodeID)

	if m["NODE_ID"] != testNodeID {
		t.Errorf("NODE_ID = %q, want %q", m["NODE_ID"], testNodeID)
	}
	if m["LINK_SECRET"] != "tunnel-secret-1" {
		t.Errorf("LINK_SECRET = %q, want %q", m["LINK_SECRET"], "tunnel-secret-1")
	}
	if m["LINK_DISCOVERY_PROOF"] != "discovery-proof-1" {
		t.Errorf("LINK_DISCOVERY_PROOF = %q, want %q", m["LINK_DISCOVERY_PROOF"], "discovery-proof-1")
	}
	if m["REDIS_ADDR"] != addr {
		t.Errorf("REDIS_ADDR = %q, want %q", m["REDIS_ADDR"], addr)
	}
	if m["REDIS_USER"] != wantUser {
		t.Errorf("REDIS_USER = %q, want %q (scoped link user, NOT plain node user)", m["REDIS_USER"], wantUser)
	}
	if m["REDIS_PASS"] != wantPass {
		t.Errorf("REDIS_PASS = %q, want %q", m["REDIS_PASS"], wantPass)
	}
	if m["REDIS_DB"] != "5" {
		t.Errorf("REDIS_DB = %q, want %q", m["REDIS_DB"], "5")
	}
	// A node in our own datacenter keeps the link on the private path, which is
	// the direct one there.
	if m["LINK_EXTERNAL"] != "false" {
		t.Errorf("LINK_EXTERNAL = %q, want false for an in-cluster node", m["LINK_EXTERNAL"])
	}
}

// On a customer machine the edge's private address is reachable too, because
// warp routes the overlay - so the link's default preference SUCCEEDS and puts
// every player's bytes through the WireGuard tunnel. The node is the only thing
// that knows which kind of machine it is on, so it has to say so.
func TestBuildLinkEnv_ExternalNodeTellsTheLink(t *testing.T) {
	origExternal := nodeExternal
	origSecret := nodeSecret
	origCluster := clusterSecret
	t.Cleanup(func() {
		nodeExternal = origExternal
		nodeSecret = origSecret
		clusterSecret = origCluster
	})
	nodeSecret = []byte("unit-test-secret-value-buildlinkenv")
	nodeExternal = true
	// Set, so this asserts the FLAG rather than the no-cluster-secret fallback.
	clusterSecret = "unit-test-cluster-secret"

	m := envMap(t, buildLinkEnv("nodeXYZ", "s", "p", "10.9.9.9:6379"))
	if m["LINK_EXTERNAL"] != "true" {
		t.Errorf("LINK_EXTERNAL = %q, want true for an external node", m["LINK_EXTERNAL"])
	}
}

// A BYON owner controls NODE_EXTERNAL and NODE_TAGS on their own machine, so
// clearing both used to hand the link the private address - which warp answers.
// Holding no CLUSTER_SECRET is the part they cannot clear their way out of.
func TestBuildLinkEnv_ByonNodeIsExternalEvenWithEveryFlagCleared(t *testing.T) {
	origExternal := nodeExternal
	origSecret := nodeSecret
	origCluster := clusterSecret
	t.Cleanup(func() {
		nodeExternal = origExternal
		nodeSecret = origSecret
		clusterSecret = origCluster
	})
	nodeSecret = []byte("unit-test-secret-value-buildlinkenv")
	nodeExternal = false
	clusterSecret = ""

	m := envMap(t, buildLinkEnv("nodeXYZ", "s", "p", "10.9.9.9:6379"))
	if m["LINK_EXTERNAL"] != "true" {
		t.Errorf("LINK_EXTERNAL = %q, want true: a node holding no CLUSTER_SECRET is BYON, whatever it says about itself", m["LINK_EXTERNAL"])
	}
}

func TestApplyPidsLimit(t *testing.T) {
	origPidsLimit := pidsLimit
	t.Cleanup(func() { pidsLimit = origPidsLimit })

	t.Run("zero (unlimited) leaves PidsLimit nil", func(t *testing.T) {
		pidsLimit = 0
		hc := &container.HostConfig{}
		applyPidsLimit(hc)
		if hc.Resources.PidsLimit != nil {
			t.Fatalf("PidsLimit = %v, want nil", *hc.Resources.PidsLimit)
		}
	})

	t.Run("positive limit is applied", func(t *testing.T) {
		pidsLimit = 512
		hc := &container.HostConfig{}
		applyPidsLimit(hc)
		if hc.Resources.PidsLimit == nil {
			t.Fatal("PidsLimit is nil, want set to 512")
		}
		if *hc.Resources.PidsLimit != 512 {
			t.Fatalf("PidsLimit = %d, want 512", *hc.Resources.PidsLimit)
		}
	})

	// Real (non-obvious) behavior: the guard is strictly "> 0", so a
	// negative value (which some Docker conventions use as an explicit
	// "unlimited" sentinel) is NOT applied here either - it just silently
	// leaves the field nil, same as 0. Flagging as a read-source note, not
	// a bug: negative pidsLimit is not a value this config path is meant to
	// produce (loaded from Redis and clamped elsewhere).
	t.Run("negative limit is not applied (guard is strictly > 0)", func(t *testing.T) {
		pidsLimit = -1
		hc := &container.HostConfig{}
		applyPidsLimit(hc)
		if hc.Resources.PidsLimit != nil {
			t.Fatalf("PidsLimit = %v, want nil for negative pidsLimit", *hc.Resources.PidsLimit)
		}
	})
}

func TestApplyIOWeight(t *testing.T) {
	origIOWeight := ioWeight
	t.Cleanup(func() { ioWeight = origIOWeight })

	cases := []struct {
		name        string
		weight      uint16
		wantApplied bool
		wantValue   uint16
	}{
		{"zero (unset) is not applied", 0, false, 0},
		{"below range (9) is not applied", 9, false, 0},
		{"lower bound (10) is applied", 10, true, 10},
		{"mid range (500) is applied", 500, true, 500},
		{"upper bound (1000) is applied", 1000, true, 1000},
		{"above range (1001) is not applied", 1001, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ioWeight = c.weight
			hc := &container.HostConfig{}
			applyIOWeight(hc)
			if hc.Resources.BlkioWeight != c.wantValue {
				t.Fatalf("BlkioWeight = %d, want %d (ioWeight=%d, applied=%v)",
					hc.Resources.BlkioWeight, c.wantValue, c.weight, c.wantApplied)
			}
		})
	}
}

// Two containers on the SAME node must not be able to compute each other's
// Redis credential. dylaris:server:<u>:input is a stdin bridge into the JVM, so
// a shared credential meant one tenant could write to a neighbour's console;
// splitting only the ACL username would not have closed that if the password
// still derived from the node alone.
func TestShipperCredentialIsPerServer(t *testing.T) {
	const node = "node-token"
	secret := []byte("a-node-secret")

	uA, uB := aclShipperUsername(node, "srv-a"), aclShipperUsername(node, "srv-b")
	if uA == uB {
		t.Errorf("two servers share the ACL username %q", uA)
	}
	pA, pB := aclShipperPassword(secret, node, "srv-a"), aclShipperPassword(secret, node, "srv-b")
	if pA == pB {
		t.Error("two servers on the same node derive the same shipper password")
	}
	// And it must still be stable, or a container would lose Redis on restart.
	if pA != aclShipperPassword(secret, node, "srv-a") {
		t.Error("shipper password is not deterministic")
	}
}
