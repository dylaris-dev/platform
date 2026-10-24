package services

import "testing"

// The CPU limit is the container's CPU time, the pin is where it may run: a
// pin smaller than the limit silently caps the server below what it was given.
func TestPinTooSmall(t *testing.T) {
	cases := []struct {
		limit    float64
		cpuset   string
		have     int
		need     int
		tooSmall bool
	}{
		{4, "0-3", 4, 4, false},
		{4, "0-7", 8, 4, false},
		{4, "0-2", 3, 4, true},
		{2.5, "0-1", 2, 3, true},
		{2.5, "0-2", 3, 3, false},
		{1, "5", 1, 1, false},
		{0, "0", 1, 0, false}, // no limit
		{4, "", 0, 0, false},  // no pin
	}
	for _, c := range cases {
		have, need, bad := PinTooSmall(c.limit, c.cpuset)
		if have != c.have || need != c.need || bad != c.tooSmall {
			t.Errorf("PinTooSmall(%g, %q) = %d, %d, %v; want %d, %d, %v", c.limit, c.cpuset, have, need, bad, c.have, c.need, c.tooSmall)
		}
	}
}
