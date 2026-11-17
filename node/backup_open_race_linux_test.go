package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The walk judged a file's identity on the listed entry and then opened the
// name, which a plugin can swap for a link in between: the open followed it to
// the node's config or an earlier archive, and the archive carried that under
// the planted name. What is opened is what is judged.
func TestAnArchiveJudgesTheFileItOpensNotTheOneItListed(t *testing.T) {
	cases := []struct {
		name   string
		target string // relative to the server root
	}{
		{"node config", ".node_config.json"},
		{"earlier archive", filepath.Join(backupDirName, "old.tar.gz")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "main"), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, tc.target)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			x := filepath.Join(root, "main", "x")
			if err := os.WriteFile(x, []byte("plain!"), 0o644); err != nil {
				t.Fatal(err)
			}
			orig := openArchiveEntry
			t.Cleanup(func() { openArchiveEntry = orig })
			openArchiveEntry = func(r *os.Root, name string, deny map[fileIdentity]bool) (*os.File, error) {
				if filepath.Base(name) == "x" {
					rel, _ := filepath.Rel(filepath.Join(root, "main"), target)
					if err := os.Remove(x); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(rel, x); err != nil {
						t.Fatal(err)
					}
				}
				return orig(r, name, deny)
			}
			var buf bytes.Buffer
			if _, err := writeServerArchive(&buf, root, filepath.Join(root, "main"), nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			for n, body := range readArchive(t, buf.Bytes()) {
				if body == "secret" {
					t.Fatalf("archive holds %s's content under %q", tc.target, n)
				}
			}
		})
	}
}
