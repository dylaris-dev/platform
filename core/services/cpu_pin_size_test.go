package services

import "testing"

// The CPU limit is the container's CPU time, the pin is where it may run. A pin
// is exactly the server's cores: fewer caps it below what it was given, more
// lets it wander over cores it has no time for.
func TestPinMismatch(t *testing.T) {
	cases := []struct {
		limit    float64
		cpuset   string
		have     int
		need     int
		mismatch bool
	}{
		{4, "0-3", 4, 4, false},
		{4, "0-7", 8, 4, true},
		{2, "0-7", 8, 2, true}, // the live shape that showed 8 cores for a 2-core server
		{4, "0-2", 3, 4, true},
		{2.5, "0-1", 2, 3, true},
		{2.5, "0-2", 3, 3, false},
		{1, "5", 1, 1, false},
		{0, "0-7", 8, 0, false}, // no limit: the pin alone decides
		{4, "", 0, 0, false},    // no pin
	}
	for _, c := range cases {
		have, need, bad := PinMismatch(c.limit, c.cpuset)
		if have != c.have || need != c.need || bad != c.mismatch {
			t.Errorf("PinMismatch(%g, %q) = %d, %d, %v; want %d, %d, %v", c.limit, c.cpuset, have, need, bad, c.have, c.need, c.mismatch)
		}
	}
}
