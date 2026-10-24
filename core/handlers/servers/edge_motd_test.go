package servers

import "testing"

// TestValidEdgeMotdMode pins the accepted edge-MOTD modes. A mode silently
// slipping through would be published to Redis and confuse the edge (which
// treats unknown non-"off"/"custom" values as "auto").
func TestValidEdgeMotdMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"auto", true},
		{"custom", true},
		{"off", true},
		{"", false},
		{"AUTO", false},
		{"disabled", false},
		{"on", false},
		{" auto", false},
	}
	for _, tt := range tests {
		if got := validEdgeMotdMode(tt.mode); got != tt.want {
			t.Errorf("validEdgeMotdMode(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}
