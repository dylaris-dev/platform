package main

import "testing"

func TestDialIdentity(t *testing.T) {
	cases := []struct {
		name, cached string
		paired       bool
		env, want    string
	}{
		{"assigned id wins over everything", "uuid-1", true, "old-name", "uuid-1"},
		{"fresh node: the hostname, NODE_ID ignored", "", false, "old-name", "host-a"},
		{"pre-2026-07 row: keeps presenting its NODE_ID", "", true, "old-name", "old-name"},
		{"paired, no id, no NODE_ID: the hostname", "", true, " ", "host-a"},
	}
	for _, c := range cases {
		if got := dialIdentity(c.cached, c.paired, c.env, "host-a"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
