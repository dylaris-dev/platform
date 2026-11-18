package beam

import (
	"testing"

	"dylaris-core/services"
)

// Z11: a node with BEAM_LAN_PORT changed listens on that port, so the ticket
// must advertise it; an older node (nothing reported) or a broken value keeps
// the default.
func TestReportedBeamLANPort(t *testing.T) {
	for _, tt := range []struct {
		name string
		hb   services.NodeHeartbeat
		ok   bool
		want string
	}{
		{"no heartbeat", services.NodeHeartbeat{BeamLANPort: "26523"}, false, "25523"},
		{"old node reports none", services.NodeHeartbeat{}, true, "25523"},
		{"changed port", services.NodeHeartbeat{BeamLANPort: "26523"}, true, "26523"},
		{"not a number", services.NodeHeartbeat{BeamLANPort: "26523x"}, true, "25523"},
		{"zero", services.NodeHeartbeat{BeamLANPort: "0"}, true, "25523"},
		{"too high", services.NodeHeartbeat{BeamLANPort: "65536"}, true, "25523"},
		{"leading zero", services.NodeHeartbeat{BeamLANPort: "026523"}, true, "25523"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportedBeamLANPort(tt.hb, tt.ok); got != tt.want {
				t.Errorf("reportedBeamLANPort = %q, want %q", got, tt.want)
			}
		})
	}
}
