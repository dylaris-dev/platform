package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The node reads its own files in the server root (the tenant's /data) as the
// root user. A link planted there, under a root container (MC_RUN_AS=0 with a
// root image), must not make that read return a file it points at: the content
// is copied back into .dylaris.json, which the tenant can read.
func TestServerRootReadsRefuseLinks(t *testing.T) {
	base := t.TempDir()
	srv := filepath.Join(base, "srv")
	if err := os.MkdirAll(filepath.Join(srv, "survival"), 0o755); err != nil {
		t.Fatal(err)
	}
	requireSymlinks(t, srv)
	// Stands in for <storage>/.node_secret and for a neighbour's config.
	if err := os.WriteFile(filepath.Join(base, ".node_secret"), []byte("deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "other.json"), []byte(`{"docker":{"container_port":4242}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srv, "survival", "inner"), []byte("survival"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		target string // "" = a regular file with body
		body   string
		active string // what readActiveSubServer must return
	}{
		{name: "regular file", body: "survival\n", active: "survival"},
		{name: "link out of the root", target: "../.node_secret"},
		{name: "absolute link out", target: filepath.Join(base, ".node_secret")},
		{name: "link inside the root", target: "survival/inner"},
		{name: "not one plain name", body: "../escape"},
		{name: "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(srv, ".active_server")
			os.Remove(p)
			switch {
			case tc.target != "":
				if err := os.Symlink(tc.target, p); err != nil {
					t.Fatal(err)
				}
			case tc.body != "":
				if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := readActiveSubServer(srv); got != tc.active {
				t.Fatalf("readActiveSubServer = %q, want %q", got, tc.active)
			}
			if tc.target != "" {
				if b, err := readServerRootFile(srv, ".active_server"); err == nil {
					t.Fatalf("read followed the link: %q", b)
				}
			}
		})
	}

	// The saved-config readers go through the same helper.
	cfg := filepath.Join(srv, ".node_config.json")
	if err := os.Symlink("../other.json", cfg); err != nil {
		t.Fatal(err)
	}
	if got := savedContainerPortIn(srv); got != 0 {
		t.Fatalf("savedContainerPortIn followed a link to a neighbour's config: port %d", got)
	}
}

// A root container can truncate one of these files to a terabyte: sparse, it
// costs no quota, and an unbounded read OOM-killed the node at every boot
// (restorePortLedger). Past serverRootFileMax the read is refused.
func TestServerRootReadIsBounded(t *testing.T) {
	srv := t.TempDir()
	for _, c := range []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{"small", 64, false},
		{"at the cap", serverRootFileMax, false},
		{"past the cap", serverRootFileMax + 1, true},
		{"sparse terabyte", 1 << 40, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(srv, ".node_config.json")
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(c.size); err != nil {
				f.Close()
				t.Skipf("cannot make a %d byte file here: %v", c.size, err)
			}
			f.Close()
			b, err := readServerRootFile(srv, ".node_config.json")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if err == nil && int64(len(b)) != c.size {
				t.Fatalf("read %d bytes, want %d", len(b), c.size)
			}
		})
	}
}
