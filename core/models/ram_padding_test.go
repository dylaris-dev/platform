package models

import "testing"

func TestEffectiveRAMPaddingMB(t *testing.T) {
	p := func(n int) *int { return &n }
	cases := []struct {
		name         string
		server, node *int
		global, want int
	}{
		{"nothing set falls to the global", nil, nil, 512, 512},
		{"node overrides global", nil, p(1024), 512, 1024},
		{"server overrides node", p(2048), p(1024), 512, 2048},
		{"server 0 is no padding, not inherit", p(0), p(1024), 512, 0},
		{"node 0 is no padding, not inherit", nil, p(0), 768, 0},
		{"global 0 is no padding", nil, nil, 0, 0},
	}
	for _, c := range cases {
		if got := EffectiveRAMPaddingMB(c.server, c.node, c.global); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestParseGlobalRAMPaddingMB(t *testing.T) {
	cases := map[string]int{
		"":      DefaultRAMPaddingMB,
		"  ":    DefaultRAMPaddingMB,
		"abc":   DefaultRAMPaddingMB,
		"-1":    DefaultRAMPaddingMB,
		"16385": DefaultRAMPaddingMB,
		"1.5":   DefaultRAMPaddingMB,
		"0":     0,
		"1024":  1024,
		"16384": 16384,
	}
	for raw, want := range cases {
		if got := ParseGlobalRAMPaddingMB(raw); got != want {
			t.Errorf("ParseGlobalRAMPaddingMB(%q) = %d, want %d", raw, got, want)
		}
	}
}
