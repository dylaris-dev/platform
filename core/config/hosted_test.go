package config

import "testing"

func TestParseHosted(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"yes", false},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{" 1 ", true},
	}
	for _, c := range cases {
		if got := parseHosted(c.in); got != c.want {
			t.Errorf("parseHosted(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
