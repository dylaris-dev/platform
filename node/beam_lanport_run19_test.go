package main

import "testing"

// Z11: the heartbeat reports the port the LAN listener binds, so Core can
// advertise it on a ticket instead of always 25523.
func TestBeamLANPortIsReported(t *testing.T) {
	for _, tt := range []struct{ env, want string }{
		{"", "25523"},
		{"26523", "26523"},
	} {
		t.Setenv("BEAM_LAN_PORT", tt.env)
		if got := beamLANPort(); got != tt.want {
			t.Errorf("BEAM_LAN_PORT=%q: beamLANPort() = %q, want %q", tt.env, got, tt.want)
		}
		data := map[string]interface{}{}
		addServerNetworkFacts(data, "", "")
		if got := data["beamLanPort"]; got != tt.want {
			t.Errorf("BEAM_LAN_PORT=%q: heartbeat beamLanPort = %v, want %q", tt.env, got, tt.want)
		}
	}
}
