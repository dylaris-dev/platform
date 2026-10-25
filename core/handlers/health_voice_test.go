package handlers

import (
	"testing"

	"dylaris-core/services"
)

func TestVoiceVerdict(t *testing.T) {
	r := func(region string, relays, used int) VoiceRegionStat {
		return VoiceRegionStat{Region: region, Relays: relays, PortsUsed: used, PortsTotal: 1000}
	}
	cases := []struct {
		name    string
		regions []VoiceRegionStat
		want    string
	}{
		{"nothing set up", nil, "disabled"},
		{"relays, room left", []VoiceRegionStat{r("eu-central", 2, 120)}, "up"},
		{"pool at the warning line", []VoiceRegionStat{r("eu-central", 2, 800)}, "degraded"},
		{"just under it", []VoiceRegionStat{r("eu-central", 2, 799)}, "up"},
		{"ports held, no relay: players hear nothing", []VoiceRegionStat{r("eu-central", 2, 10), r("us", 0, 3)}, "down"},
		{"a relay-less region nobody uses", []VoiceRegionStat{r("eu-central", 2, 10), r("us", 0, 0)}, "up"},
		{"down outranks degraded", []VoiceRegionStat{r("eu-central", 2, 900), r("us", 0, 1)}, "down"},
		{"one of two edges without a relay", []VoiceRegionStat{{Region: "eu-central", Relays: 1, PortsUsed: 5, PortsTotal: 1000, EdgesWithoutRelay: 1}}, "degraded"},
		{"an edge without a relay nobody uses", []VoiceRegionStat{{Region: "eu-central", Relays: 1, PortsUsed: 0, PortsTotal: 1000, EdgesWithoutRelay: 1}}, "up"},
	}
	for _, c := range cases {
		got, _, reason, _ := voiceVerdict(c.regions)
		if got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, reason, c.want)
		}
	}
}

// An edge counts as covered only by a relay on its own address and region.
func TestRelaylessEdges(t *testing.T) {
	edges := []services.GatewayEdgeInfo{
		{Region: "eu-central", IP: "1.1.1.1", Status: "online"},
		{Region: "eu-central", IP: "2.2.2.2", Status: "online"},
		{Region: "eu-central", IP: "3.3.3.3", Status: "offline"},
		{Region: "us", IP: "4.4.4.4"},
		{Region: "", IP: "5.5.5.5"},
	}
	relays := []services.VoiceRelay{{Region: "eu-central", IP: "1.1.1.1"}, {Region: "us", IP: "2.2.2.2"}}
	got := relaylessEdges(edges, relays)
	if got["eu-central"] != 1 || got["us"] != 1 || len(got) != 2 {
		t.Fatalf("relaylessEdges = %v, want eu-central 1 (2.2.2.2; the offline one does not count), us 1", got)
	}
}
