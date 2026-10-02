package main

import (
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

// Egress used to be unfiltered: a server's code reached the cloud metadata
// service, Core's own ports and the node's services directly.
func TestEgressRulesBlockThePlatformButNotTheGamePort(t *testing.T) {
	rs := netPolicyRulesetWith([]string{"10.20.0.5"}, &netEgress{overlay: "10.20.0.0/16", redisIP: "10.0.0.10", redisPort: 6379, gamePort: 25565})
	if p := os.Getenv("DUMP_RULESET"); p != "" {
		os.WriteFile(p, []byte(rs), 0o644)
	}
	for _, want := range []string{
		"chain output",
		"ip daddr 10.0.0.10 tcp dport 6379 accept",
		"ip daddr 169.254.0.0/16 drop",
		"ip daddr 10.20.0.0/16 tcp dport { 6379, 25500-25564, 25566-25599 } drop",
		"ip daddr 10.20.0.0/16 accept",
		"10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, rs)
		}
	}
	// The Redis allow comes before the private-range drop, or the log-shipper
	// is cut off.
	if strings.Index(rs, "10.0.0.10 tcp dport 6379 accept") > strings.Index(rs, "172.16.0.0/12") {
		t.Error("the private-range drop comes before the Redis allow")
	}
	// A value that does not parse drops the chain, never the network.
	if bad := netPolicyRulesetWith(nil, &netEgress{overlay: "x; flush ruleset", redisIP: "10.0.0.10", redisPort: 6379, gamePort: 25565}); strings.Contains(bad, "chain output") || strings.Contains(bad, "flush") {
		t.Errorf("an unparsable overlay reached the ruleset:\n%s", bad)
	}
}

// The tenant's code ran with Docker's default capabilities and could gain more
// through a setuid binary in an image of its choosing; /tmp wrote to the host
// disk past the server's limit.
func TestATenantContainerHoldsNoPrivilegeAndBoundedScratch(t *testing.T) {
	hc := &container.HostConfig{}
	hardenTenantContainer(hc)
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want ALL", hc.CapDrop)
	}
	nnp := false
	for _, o := range hc.SecurityOpt {
		nnp = nnp || o == "no-new-privileges:true"
	}
	if !nnp {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", hc.SecurityOpt)
	}
	for _, p := range []string{"/tmp", "/var/tmp", "/home/dylaris"} {
		if !strings.Contains(hc.Tmpfs[p], "size=") {
			t.Errorf("%s is not a bounded tmpfs: %q", p, hc.Tmpfs[p])
		}
	}
}

// Unlimited processes was the default: one fork bomb exhausted the host.
func TestTheProcessCapHasADefault(t *testing.T) {
	r, f, p, cp, io, pids := getModes()
	t.Cleanup(func() { setModes(r, f, p, cp, io, pids) })
	setModes(r, f, p, cp, io, defaultPidsLimit)
	hc := &container.HostConfig{}
	applyPidsLimit(hc)
	if hc.Resources.PidsLimit == nil || *hc.Resources.PidsLimit != defaultPidsLimit {
		t.Fatalf("PidsLimit = %v, want %d", hc.Resources.PidsLimit, defaultPidsLimit)
	}
}
