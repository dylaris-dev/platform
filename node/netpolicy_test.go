package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

// The ruleset is the whole security boundary, so what it must NOT contain
// matters more than what it does.
func TestNetPolicyRulesetShape(t *testing.T) {
	rs := netPolicyRuleset([]string{"10.20.12.27", "10.20.12.22"})

	musts := []struct{ frag, why string }{
		{"policy drop;", "the default is deny, or the allowlist is decoration"},
		{"ct state established,related accept", "without it the server cannot talk to Redis: its own replies are dropped"},
		{"iif lo accept", "a server talking to itself is not server-to-server traffic"},
		{"ip protocol icmp accept", "path-MTU discovery rides on ICMP; black-holing it looks like a random disconnect"},
		{"ip saddr 10.20.12.22 accept", "an allowed peer has to be in there"},
		{"ip saddr 10.20.12.27 accept", "an allowed peer has to be in there"},
	}
	for _, m := range musts {
		if !strings.Contains(rs, m.frag) {
			t.Errorf("ruleset is missing %q: %s\n%s", m.frag, m.why, rs)
		}
	}

	// Ours alone. Docker owns "ip nat" in the same namespace and a container
	// that lost it loses its own outbound address translation.
	if strings.Contains(rs, "ip nat") || strings.Contains(rs, "flush ruleset") {
		t.Errorf("ruleset touches something that is not ours:\n%s", rs)
	}
}

// A peer address arrives from Core, over Redis, and is pasted into a ruleset.
// Anything that is not an address must never reach that file - one crafted
// entry would otherwise be able to open the server to everything.
func TestNetPolicyRulesetRefusesAnythingThatIsNotAnAddress(t *testing.T) {
	poison := []string{
		"10.0.0.1 accept\n    ip saddr 0.0.0.0/0",
		"0.0.0.0/0",
		"; drop",
		"mc_deadbeef",
		"}\n chain output { type filter hook output priority 0; policy accept; }",
		"",
		"   ",
	}
	rs := netPolicyRuleset(poison)

	if strings.Contains(rs, "0.0.0.0/0") {
		t.Errorf("an allow-everything slipped through:\n%s", rs)
	}
	if strings.Contains(rs, "chain output") {
		t.Errorf("the ruleset was extended by its own input:\n%s", rs)
	}
	if n := strings.Count(rs, "saddr"); n != 0 {
		t.Errorf("got %d saddr rules from input containing no valid address:\n%s", n, rs)
	}
	// And it must still be a closed ruleset rather than an empty one.
	if !strings.Contains(rs, "policy drop;") {
		t.Errorf("poisoned input produced a ruleset without a default deny:\n%s", rs)
	}
}

func TestNetPolicyRulesetSeparatesV4AndV6(t *testing.T) {
	rs := netPolicyRuleset([]string{"10.1.2.3", "fd00::5"})
	if !strings.Contains(rs, "ip saddr 10.1.2.3 accept") {
		t.Errorf("v4 address not rendered as ip saddr:\n%s", rs)
	}
	// nft rejects the whole file if a v6 literal is given to an `ip` match, so
	// getting this wrong is not a partial failure - it is no rules at all,
	// which fails OPEN.
	if !strings.Contains(rs, "ip6 saddr fd00::5 accept") {
		t.Errorf("v6 address not rendered as ip6 saddr:\n%s", rs)
	}
}

// The reconciler runs this every 15s against a container it may already have
// configured, so identical input must produce identical output - otherwise
// every pass rewrites the ruleset and the log fills with churn.
func TestNetPolicyRulesetIsStableAndDeduped(t *testing.T) {
	a := netPolicyRuleset([]string{"10.0.0.2", "10.0.0.1", "10.0.0.2"})
	b := netPolicyRuleset([]string{"10.0.0.1", "10.0.0.2"})
	if a != b {
		t.Errorf("order and duplicates changed the ruleset:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	if n := strings.Count(a, "10.0.0.2"); n != 1 {
		t.Errorf("duplicate address rendered %d times", n)
	}
}

// Three states, and the one that must never be silent is "on, and not
// holding": fail-open means a server that could not be given rules is running
// wide open right now.
func TestNetPolicyNotice(t *testing.T) {
	tests := []struct {
		name        string
		enforced    bool
		external    bool
		applied     int
		failures    int
		reason      string
		unpublished bool
		want        func(string) bool
		why         string
	}{
		{
			name: "enforced and holding", enforced: true, applied: 3,
			want: func(s string) bool { return s == "" },
			why:  "nothing to say; the badge already says it",
		},
		{
			name: "off because the machine is not ours", external: true,
			want: func(s string) bool { return strings.Contains(s, "not one of our machines") },
			why:  "a BYON operator must not be told to fix something that is deliberate",
		},
		{
			name: "off on one of ours", enforced: false, external: false,
			want: func(s string) bool {
				return strings.HasPrefix(s, "Off") && !strings.Contains(s, "not one of our machines")
			},
			why: "off on our own hardware is a different fact from off on a customer's",
		},
		{
			name: "one server left open", enforced: true, applied: 2, failures: 1, reason: "abc12345: helper exited 1",
			want: func(s string) bool {
				return strings.Contains(s, "1 server ") && strings.Contains(s, "reachable from any other server") &&
					strings.Contains(s, "helper exited 1")
			},
			why: "the count, the consequence and the cause; fail-open is only acceptable while it is loud",
		},
		{
			name: "several left open", enforced: true, applied: 0, failures: 4, reason: "x: boom",
			want: func(s string) bool { return strings.Contains(s, "4 servers ") },
			why:  "plural, because a sentence that reads wrong reads as a bug in the panel",
		},
		{
			// Waiting is not failing. Nothing is enforced until Core has
			// published once - on purpose, so a Core that knows nothing about
			// this cannot narrow a running network - and the sentence has to say
			// that rather than blame something.
			name: "no policy published yet", enforced: true, unpublished: true,
			want: func(s string) bool {
				return strings.HasPrefix(s, "Waiting") && strings.Contains(s, "accept any other server")
			},
			why: "an operator must be able to tell 'not yet' from 'broken'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := netPolicyNotice(tt.enforced, tt.external, tt.applied, tt.failures, tt.reason, tt.unpublished)
			if !tt.want(got) {
				t.Errorf("notice = %q (%s)", got, tt.why)
			}
		})
	}
}

// The counter is written from the reconciler and read from the heartbeat.
func TestNetPolicyStateKeepsCountAndLastReason(t *testing.T) {
	var s netPolicyState
	if a, f, r, unpub := s.snapshot(); a != 0 || f != 0 || r != "" || unpub {
		t.Fatalf("fresh state = (%d,%d,%q,%v)", a, f, r, unpub)
	}
	s.recordFailure("aaaaaaaa-1111", fmt.Errorf("first"))
	s.recordFailure("bbbbbbbb-2222", fmt.Errorf("second"))
	s.recordApplied(5)
	a, f, r, _ := s.snapshot()
	if a != 5 || f != 2 {
		t.Errorf("snapshot = (%d,%d), want (5,2)", a, f)
	}
	if !strings.Contains(r, "second") || !strings.Contains(r, "bbbbbbbb") {
		t.Errorf("reason = %q, want the most recent one with its server", r)
	}
	// A nil error is not a failure. A counter that success can increment is a
	// counter nobody can act on.
	s.recordFailure("cccc", nil)
	if _, f2, _, _ := s.snapshot(); f2 != 2 {
		t.Errorf("a nil error was counted: %d", f2)
	}
}

// The key has to sit under the prefix the node's own Redis ACL already grants
// (~dylaris:node:<id>:*). A key outside it is NOPERM, and a NOPERM read looks
// exactly like an empty policy - the failure would be silent and would leave
// every server on node+Link forever.
func TestNetPolicyKeyIsInsideTheNodesOwnGrant(t *testing.T) {
	id := "597de090-d6b5-4c66-b617-d13b676ecb53"
	got := netPolicyKey(id)
	want := "dylaris:node:" + id + ":netpolicy"
	if got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "dylaris:node:"+id+":") {
		t.Errorf("key %q is outside the granted prefix", got)
	}
}

// endpoints builds a NetworkSettingsSummary the way ContainerList actually
// returns one, so the test data is shaped like the real thing rather than
// like whatever is convenient to assert on.
func endpoints(byNetwork map[string]network.EndpointSettings) *container.NetworkSettingsSummary {
	m := make(map[string]*network.EndpointSettings, len(byNetwork))
	for name, ep := range byNetwork {
		ep := ep
		m[name] = &ep
	}
	return &container.NetworkSettingsSummary{Networks: m}
}

// TestLinkContainerAddrs covers the helper the brief asks for: given
// ContainerList summaries, it returns exactly the addresses of the Link
// containers among them.
func TestLinkContainerAddrs(t *testing.T) {
	tests := []struct {
		name         string
		containers   []container.Summary
		want         []string
		wantAddrless int
	}{
		{
			name: "the node-managed link",
			containers: []container.Summary{{
				Names:           []string{"/dylaris_link"},
				Image:           "ghcr.io/dylaris-dev/gateway-link:latest",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.5"}}),
			}},
			want: []string{"10.0.0.5"},
		},
		{
			name: "a stack task, generated name",
			containers: []container.Summary{{
				Names:           []string{"/dylaris-prod_link.abc.xyz"},
				Image:           "ghcr.io/dylaris-dev/gateway-link@sha256:deadbeef",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.7"}}),
			}},
			want: []string{"10.0.0.7"},
		},
		{
			name: "two links at once, start-first overlap",
			containers: []container.Summary{
				{
					Names:           []string{"/dylaris_link"},
					Image:           "ghcr.io/dylaris-dev/gateway-link:latest",
					NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.5"}}),
				},
				{
					Names:           []string{"/dylaris-prod_link.def.uvw"},
					Image:           "ghcr.io/dylaris-dev/gateway-link@sha256:deadbeef",
					NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.9"}}),
				},
			},
			want: []string{"10.0.0.5", "10.0.0.9"},
		},
		{
			name: "a custom LINK_IMAGE registry but the legacy name",
			containers: []container.Summary{{
				Names:           []string{"/dylaris_link"},
				Image:           "registry.example.test/private/link:2026.09",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.11"}}),
			}},
			want: []string{"10.0.0.11"},
		},
		{
			name: "an mc server and the node itself are excluded",
			containers: []container.Summary{
				{
					Names:           []string{"/mc_7f3a"},
					Image:           "itzg/minecraft-server:latest",
					NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.20"}}),
				},
				{
					Names:           []string{"/dylaris_node"},
					Image:           "ghcr.io/dylaris-dev/platform-node:latest",
					NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.2"}}),
				},
			},
			want: nil,
		},
		{
			name: "ipv4 and global ipv6 on two networks, all addresses",
			containers: []container.Summary{{
				Names: []string{"/dylaris_link"},
				Image: "ghcr.io/dylaris-dev/gateway-link:latest",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{
					"dylaris_net": {IPAddress: "10.0.0.5"},
					"ipv6_net":    {GlobalIPv6Address: "2001:db8::5"},
				}),
			}},
			want: []string{"10.0.0.5", "2001:db8::5"},
		},
		{
			// The case the label exists for. Neither test that came before it
			// matches, and the failure is silent: every server drops this Link
			// while the container itself looks healthy.
			name: "a mirrored image under an orchestrator-generated name, found by the label",
			containers: []container.Summary{{
				Names:           []string{"/prod_link.abc.xyz"},
				Image:           "registry.example.test/private/link:2026.09",
				Labels:          map[string]string{"com.dylaris.role": "link"},
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.13"}}),
			}},
			want: []string{"10.0.0.13"},
		},
		{
			// The same container WITHOUT the label, to keep the case above
			// honest: it must be the label doing the work and not the name.
			name: "the same one with no label is still not a link",
			containers: []container.Summary{{
				Names:           []string{"/prod_link.abc.xyz"},
				Image:           "registry.example.test/private/link:2026.09",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"dylaris_net": {IPAddress: "10.0.0.13"}}),
			}},
			want: nil,
		},
		{
			// A host-networked Link: recognised, but its endpoint carries no
			// address, so it can never appear in a rule. Counted rather than
			// skipped, because linkCount reports it as a working Link.
			name: "a host-networked link is counted as address-less",
			containers: []container.Summary{{
				Names:           []string{"/dylaris_link"},
				Image:           "ghcr.io/dylaris-dev/gateway-link:latest",
				NetworkSettings: endpoints(map[string]network.EndpointSettings{"host": {}}),
			}},
			want:         nil,
			wantAddrless: 1,
		},
		{
			name: "empty NetworkSettings, no panic",
			containers: []container.Summary{{
				Names:           []string{"/dylaris_link"},
				Image:           "ghcr.io/dylaris-dev/gateway-link:latest",
				NetworkSettings: &container.NetworkSettingsSummary{},
			}},
			want:         nil,
			wantAddrless: 1,
		},
		{
			name: "nil NetworkSettings, no panic",
			containers: []container.Summary{{
				Names:           []string{"/dylaris_link"},
				Image:           "ghcr.io/dylaris-dev/gateway-link:latest",
				NetworkSettings: nil,
			}},
			want:         nil,
			wantAddrless: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, addrless := linkContainerAddrs(tt.containers)
			if addrless != tt.wantAddrless {
				t.Fatalf("linkContainerAddrs addrless = %d, want %d", addrless, tt.wantAddrless)
			}
			sort.Strings(got)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("linkContainerAddrs = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("linkContainerAddrs = %v, want %v", got, want)
				}
			}
		})
	}
}
