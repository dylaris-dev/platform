package handlers

import "testing"

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
	}
	for _, c := range cases {
		got, _, reason, _ := voiceVerdict(c.regions)
		if got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, reason, c.want)
		}
	}
}
