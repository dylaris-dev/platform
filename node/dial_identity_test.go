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

// R01: a node paired before Core assigned ids dials as its NODE_ID and has no
// .node_id, and nothing ever saved it, so removing NODE_ID made it dial as the
// hostname, which Core does not know. Once Core accepts its secret proof for
// that name, the name is saved, and only then.
func TestRememberLegacyIdentity(t *testing.T) {
	cases := []struct {
		name, cached, env, token, assigned string
		proved                             bool
		want                               string
	}{
		{"legacy node accepted: NODE_ID saved", "", "old-name", "old-name", "", true, "old-name"},
		{"accepted with its own name echoed back", "", "old-name", "old-name", "old-name", true, "old-name"},
		{"no secret proof: nothing to vouch for the name", "", "old-name", "old-name", "", false, ""},
		{"dialled as the hostname, not NODE_ID", "", "old-name", "host-a", "", true, ""},
		{"Core assigned a different id", "", "old-name", "old-name", "uuid-9", true, ""},
		{"already has an id: left alone", "uuid-1", "uuid-1", "uuid-1", "", true, "uuid-1"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if c.cached != "" {
			if err := saveNodeID(dir, c.cached); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("NODE_ID", c.env)
		rememberLegacyIdentity(dir, c.token, c.proved, c.assigned)
		if got, _ := cachedNodeID(dir); got != c.want {
			t.Errorf("%s: .node_id = %q, want %q", c.name, got, c.want)
		}
	}
}
