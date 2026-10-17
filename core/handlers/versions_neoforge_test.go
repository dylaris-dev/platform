package handlers

import "testing"

func TestNeoForgeMajor(t *testing.T) {
	tests := []struct {
		build string
		want  string
		ok    bool
	}{
		{"21.1.262", "1.21", true},
		{"20.4.237", "1.20", true},
		// Since MC 26.x the build follows Minecraft itself; the old rule said "1.26".
		{"26.3.0.89", "26.3", true},
		{"26.1.0.19", "26.1", true},
		// Pre-releases are not offered, like PaperMC's.
		{"26.1.0.19-beta", "", false},
		{"26.1.0.0-alpha.4+snapshot-1", "", false},
		// The April-fools builds made a Minecraft "1.0" appear.
		{"0.25w14craftmine.3", "", false},
		{"garbage", "", false},
	}
	for _, tt := range tests {
		got, ok := neoForgeMajor(tt.build)
		if got != tt.want || ok != tt.ok {
			t.Errorf("neoForgeMajor(%q) = %q, %v; want %q, %v", tt.build, got, ok, tt.want, tt.ok)
		}
	}
}
