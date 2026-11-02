package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The link-local drop is the one egress rule written in every variant, and it
// comes before anything that accepts: a metadata request a container opened
// before its rules landed is not kept alive by "established".
func TestTheMetadataDropIsAlwaysFirst(t *testing.T) {
	full := &netEgress{overlay: "10.20.0.0/16", redisIP: "10.0.0.10", redisPort: 6379, gamePort: 25565, gateway: "10.20.0.1"}
	for name, rs := range map[string]string{
		"separated, full egress":   renderNetPolicy(true, []string{"10.20.0.5"}, nil, full),
		"not separated, full":      renderNetPolicy(false, nil, nil, full),
		"egress unknown":           renderNetPolicy(true, []string{"10.20.0.5"}, nil, nil),
		"single owner, link-local": renderNetPolicy(false, nil, nil, nil),
		"unparsable egress values": renderNetPolicy(false, nil, nil, &netEgress{overlay: "nope"}),
	} {
		drop := strings.Index(rs, "ip daddr 169.254.0.0/16 drop")
		if drop < 0 || !strings.Contains(rs, "ip6 daddr fd00:ec2::254 drop") {
			t.Errorf("%s: no metadata drop:\n%s", name, rs)
			continue
		}
		if est := strings.Index(rs[strings.Index(rs, "chain output"):], "established"); est >= 0 && strings.Index(rs, "chain output")+est < drop {
			t.Errorf("%s: established accepted before the metadata drop:\n%s", name, rs)
		}
	}
}

// Not separating a machine's servers means no input chain at all: a
// single-owner machine must keep every port of its servers reachable.
func TestNoSeparationWritesNoIngressChain(t *testing.T) {
	rs := renderNetPolicy(false, []string{"10.20.0.5"}, []publishedPort{{"tcp", 25565}}, nil)
	if strings.Contains(rs, "chain input") || strings.Contains(rs, "policy drop") {
		t.Errorf("ingress written without separation:\n%s", rs)
	}
	// Nor the LAN drop: a customer's own servers keep reaching their own LAN.
	if strings.Contains(rs, "192.168.0.0/16") {
		t.Errorf("the LAN drop is written for a single-owner machine:\n%s", rs)
	}
}

// M2: the host behind a bridge, UDP to the platform's ports, Postgres and
// private IPv6 used to be reachable; DNS has to stay reachable wherever the
// host's resolver lives, or a home machine's servers cannot resolve Mojang.
func TestTheFullEgressChainClosesTheHostAndKeepsDNS(t *testing.T) {
	rs := renderNetPolicy(true, nil, nil, &netEgress{overlay: "172.30.0.0/16", redisIP: "172.30.0.1", redisPort: 25560, gamePort: 25565, gateway: "172.30.0.1"})
	order := []string{
		"ip daddr 172.30.0.1 tcp dport 25560 accept", // warp proxy Redis on the gateway first
		"udp dport 53 accept",
		"tcp dport 53 accept",
		"ip daddr 172.30.0.1 drop", // then the rest of the host
		"ip daddr 172.30.0.0/16 udp dport { 5432, 6379, 25500-25564, 25566-25599 } drop",
		"ip daddr 172.30.0.0/16 accept",
		"192.168.0.0/16",
		"ip6 daddr { fc00::/7, fe80::/10 } meta l4proto { tcp, udp } drop",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(rs, want)
		if i < 0 {
			t.Fatalf("ruleset lacks %q:\n%s", want, rs)
		}
		if i < last {
			t.Fatalf("%q is out of order:\n%s", want, rs)
		}
		last = i
	}
}

func TestTheHostRulesetDropsOnlyTheMetadataAddress(t *testing.T) {
	rs := hostRuleset("10.20.0.0/16", true)
	for _, want := range []string{
		"table inet dylaris_host {",
		"type filter hook forward priority -10; policy accept;",
		"ip saddr 10.20.0.0/16 ip daddr 169.254.169.254 drop",
		`iifname "docker_gwbridge" ip daddr 169.254.169.254 drop`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("host ruleset lacks %q:\n%s", want, rs)
		}
	}
	if strings.Contains(rs, "table inet dylaris {") || strings.Contains(rs, "flush") {
		t.Errorf("host ruleset touches something not its own:\n%s", rs)
	}
	if got := hostRuleset("bad; flush ruleset", false); got != "" {
		t.Errorf("an unparsable subnet produced a host ruleset:\n%s", got)
	}
	if got := hostRuleset("10.20.0.0/16", false); strings.Contains(got, "gwbridge") {
		t.Errorf("the gwbridge rule is written off a Swarm node:\n%s", got)
	}
}

// H2: an external machine separates its servers only when Core says they
// belong to more than one owner, or the operator forces it. An older Core
// never writes the flag, so a new node on it changes nothing but egress.
func TestExternalMachinesSeparateByTenancy(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()

	dm := &DockerManager{external: true}
	if plan := dm.planNetPolicy(ctx, rdb, "tok"); plan.separate || plan.eg != nil || dm.netPolicySeparates.Load() {
		t.Fatalf("separated without Core's word: %+v", plan)
	}
	mr.Set(netPolicyTenancyKey("tok"), "shared")
	if !dm.sharedTenancy(ctx, rdb, "tok") {
		t.Fatal("the tenancy flag was not read")
	}
	mr.Del(netPolicyTenancyKey("tok"))
	if dm.sharedTenancy(ctx, rdb, "tok") {
		t.Fatal("a removed flag still separates")
	}
	// A Redis failure keeps the last answer instead of dropping separation.
	mr.Set(netPolicyTenancyKey("tok"), "shared")
	dm.sharedTenancy(ctx, rdb, "tok")
	mr.Close()
	if !dm.sharedTenancy(ctx, rdb, "tok") {
		t.Fatal("a Redis error switched separation off")
	}
}

// The start gate: env only where rules are applied, the marker removed before
// a start and written after it, also on a node that applies nothing.
func TestTheStartGate(t *testing.T) {
	dm := &DockerManager{}
	if env := dm.netGateContainerEnv(); env != nil {
		t.Fatalf("gate env on a node that applies no rules: %v", env)
	}
	dm.netPolicyRDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { dm.netPolicyRDB.Close() })
	if env := dm.netGateContainerEnv(); len(env) != 1 || env[0] != netGateEnv+"=/data/"+netGateFile {
		t.Fatalf("gate env = %v", env)
	}

	dir := t.TempDir()
	dm2 := &DockerManager{localDataPath: dir}
	uuid := "11111111-2222-3333-4444-555555555555"
	marker := filepath.Join(dm2.resolveLocalServerPath(uuid), netGateFile)
	os.MkdirAll(filepath.Dir(marker), 0o755)
	os.WriteFile(marker, nil, 0o644)
	dm2.closeNetGate(uuid)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker survived closeNetGate: %v", err)
	}
	dm2.openNetGate(uuid)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker not written by openNetGate: %v", err)
	}
	if !isProtectedFile(netGateFile) {
		t.Error("the marker can be written through the file manager")
	}
}

// H1, second layer: an oversized key the shipper credential can write is
// deleted, a console stream gets a TTL that is re-armed every pass, and a key
// that already has a TTL keeps it.
func TestTheShipperKeyGuard(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	keys := shipperGuardedKeys("u1", []string{"lobby"})
	byName := map[string]guardedKey{}
	for _, k := range keys {
		byName[strings.TrimPrefix(k.key, "dylaris:server:u1:")] = k
	}
	for _, want := range []string{"logs", "logs:lobby", "java-heap", "input", "log_filter_rcon", "stop-requested"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("key %q is not guarded: %v", want, keys)
		}
	}

	heap := byName["java-heap"]
	mr.Set(heap.key, strings.Repeat("x", shipperSmallCap+1))
	if !guardKey(ctx, rdb, heap) || mr.Exists(heap.key) {
		t.Fatal("an oversized key was not deleted")
	}

	stop := byName["stop-requested"]
	mr.Set(stop.key, "1")
	mr.SetTTL(stop.key, 2*time.Minute)
	guardKey(ctx, rdb, stop)
	if ttl := mr.TTL(stop.key); ttl != 2*time.Minute {
		t.Fatalf("an existing TTL was replaced: %s", ttl)
	}
	in := byName["input"]
	mr.Lpush(in.key, "say hi")
	guardKey(ctx, rdb, in)
	if ttl := mr.TTL(in.key); ttl != in.ttl {
		t.Fatalf("a key without TTL got %s, want %s", ttl, in.ttl)
	}

	logs := byName["logs"]
	mr.XAdd(logs.key, "*", []string{"line", "hello"})
	mr.SetTTL(logs.key, time.Minute)
	guardKey(ctx, rdb, logs)
	if ttl := mr.TTL(logs.key); ttl != logs.ttl {
		t.Fatalf("the console stream TTL was not re-armed: %s", ttl)
	}
}
