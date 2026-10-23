package main

import "testing"

func TestParseProcStatCPU(t *testing.T) {
	cases := []struct {
		line string
		id   int
		want coreTicks
		ok   bool
	}{
		// user nice system idle iowait irq softirq steal guest guest_nice
		{"cpu3 100 5 20 800 50 3 2 0 40 0", 3, coreTicks{busy: 130, total: 980}, true},
		{"cpu 900 0 0 100 0 0 0 0", 0, coreTicks{}, false},
		{"intr 12345 0 0", 0, coreTicks{}, false},
		{"cpu12 1 2 3 4", 0, coreTicks{}, false},
		{"cpu1 10 0 10 x 0 0 0 0", 0, coreTicks{}, false},
	}
	for _, c := range cases {
		id, got, ok := parseProcStatCPU(c.line)
		if ok != c.ok || (ok && (id != c.id || got != c.want)) {
			t.Errorf("%q: got (%d, %+v, %v), want (%d, %+v, %v)", c.line, id, got, ok, c.id, c.want, c.ok)
		}
	}
}

func TestCoreLoad(t *testing.T) {
	prev := map[int]coreTicks{0: {busy: 100, total: 1000}, 1: {busy: 50, total: 1000}, 2: {busy: 10, total: 100}}
	cur := map[int]coreTicks{0: {busy: 150, total: 1100}, 1: {busy: 50, total: 1000}, 3: {busy: 5, total: 10}}
	got := coreLoad(prev, cur)
	if len(got) != 1 || got[0] != 50 {
		t.Fatalf("got %v, want only core 0 at 50%% (core 1 did not move, 2 went offline, 3 is new)", got)
	}
}
