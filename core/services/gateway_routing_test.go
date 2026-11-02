package services

import "testing"

type settingsOnly map[string]string

func (s settingsOnly) GetSetting(k string) (string, error) { return s[k], nil }

// GatewayRoutingEnabled is the ONLY answer to "is the gateway routing traffic": every
// gateway write gate, the migration orchestrator and the rebalance worker all
// ask it, and the panel mirrors it (panel/src/lib/api/types.ts isGatewayRouting).
// A second stored flag once shadowed this and the two sides disagreed in both
// directions, so pin the rule here.
func TestGatewayEnabledReadsRoutingModeOnly(t *testing.T) {
	tests := []struct {
		name string
		kv   map[string]string
		want bool
	}{
		{"unset defaults to ip_port", map[string]string{}, false},
		{"ip_port", map[string]string{"routing_mode": "ip_port"}, false},
		{"gateway", map[string]string{"routing_mode": "gateway"}, true},
		{"both", map[string]string{"routing_mode": "both"}, true},
		{"unparseable value is off", map[string]string{"routing_mode": "nonsense"}, false},
		{"a stored feature flag cannot turn it on", map[string]string{
			"feature_gateway_enabled": "true",
		}, false},
		{"a stored feature flag cannot turn it off", map[string]string{
			"routing_mode":            "gateway",
			"feature_gateway_enabled": "false",
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GatewayRoutingEnabled(settingsOnly(tt.kv)); got != tt.want {
				t.Fatalf("GatewayRoutingEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
