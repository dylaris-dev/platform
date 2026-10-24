package nodes

import (
	"strings"
	"testing"
)

// The label becomes a node's display name, so it must stay one short line.
func TestValidEnrollLabel(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"My Home PC", true},
		{"home-desktop", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{"two\nlines", false},
		{"tab\there", false},
		{"äöü-Rechner", true},
	} {
		if got := validEnrollLabel(c.in); got != c.want {
			t.Errorf("%q: %v, want %v", c.in, got, c.want)
		}
	}
}
