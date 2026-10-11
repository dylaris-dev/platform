package main

import "testing"

// The panel draws CPU on a fixed 0-100 axis, so the divisor must be the number
// of cores the container can actually use.
func TestCPUDivisor(t *testing.T) {
	tests := []struct {
		name      string
		nano      int64
		cpuset    string
		hostCores int
		want      float64
	}{
		{"quota only", 4e9, "", 16, 4},
		{"fractional quota", 1.5e9, "", 16, 1.5},
		{"cpuset only", 0, "0-3", 16, 4},
		{"cpuset list", 0, "0,2,5-6", 16, 4},
		{"neither: host cores", 0, "", 8, 8},
		{"quota above cpuset: cpuset caps", 8e9, "0-1", 16, 2},
		{"quota below cpuset: quota caps", 2e9, "0-7", 16, 2},
		{"quota above host cores: host caps", 32e9, "", 8, 8},
		{"unparseable cpuset falls back to host", 0, "x", 8, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cpuDivisor(tt.nano, tt.cpuset, tt.hostCores); got != tt.want {
				t.Errorf("cpuDivisor(%d, %q, %d) = %v, want %v", tt.nano, tt.cpuset, tt.hostCores, got, tt.want)
			}
		})
	}
}
