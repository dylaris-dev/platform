package handlers

import (
	"testing"

	"dylaris-core/services"
)

func TestBedrockVerdict(t *testing.T) {
	entry := func(region string) services.BedrockEntry {
		return services.BedrockEntry{ID: "b-" + region, Region: region}
	}
	cases := []struct {
		name    string
		enabled map[string]int
		entries []services.BedrockEntry
		want    string
	}{
		{"no server has Bedrock", nil, []services.BedrockEntry{entry("eu")}, "disabled"},
		{"no server, no entry", nil, nil, "disabled"},
		{"enabled, entry running", map[string]int{"eu": 2}, []services.BedrockEntry{entry("eu")}, "up"},
		{"enabled, no entry in that region", map[string]int{"eu": 1}, []services.BedrockEntry{entry("us")}, "degraded"},
		{"enabled, no entries at all", map[string]int{"eu": 1}, nil, "degraded"},
		{"one region covered, one not", map[string]int{"eu": 1, "us": 3}, []services.BedrockEntry{entry("eu")}, "degraded"},
		{"an entry in a region nobody uses", map[string]int{"eu": 1}, []services.BedrockEntry{entry("eu"), entry("us")}, "up"},
	}
	for _, c := range cases {
		got, _, reason, items := bedrockVerdict(c.enabled, c.entries)
		if got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.name, got, reason, c.want)
		}
		if got == "degraded" && reason == "" {
			t.Errorf("%s: degraded without a reason", c.name)
		}
		if len(items) != len(c.enabled) {
			t.Errorf("%s: %d items, want one per enabled region (%d)", c.name, len(items), len(c.enabled))
		}
	}
}
