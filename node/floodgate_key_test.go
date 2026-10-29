package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var testFloodgateKey = []byte("0123456789abcdef")

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func present(v string) func() (string, bool, error) {
	return func() (string, bool, error) { return v, true, nil }
}

func TestApplyFloodgateKey(t *testing.T) {
	absent := func() (string, bool, error) { return "", false, nil }
	failing := func() (string, bool, error) { return "", false, errors.New("redis down") }

	tests := []struct {
		name     string
		layout   string // directory that marks the layout
		existing string // pre-existing key file, relative
		fetch    func() (string, bool, error)
		wantFile string // relative; "" means nothing written
		wantKey  []byte
	}{
		{"paper layout", "plugins", "", present(b64(testFloodgateKey)), "plugins/floodgate/key.pem", testFloodgateKey},
		{"mods layout", "mods", "", present(b64(testFloodgateKey)), "config/floodgate/key.pem", testFloodgateKey},
		{"overwrites a different key", "plugins", "plugins/floodgate/key.pem", present(b64(testFloodgateKey)), "plugins/floodgate/key.pem", testFloodgateKey},
		{"absent key writes nothing", "plugins", "", absent, "", nil},
		{"redis error writes nothing", "plugins", "", failing, "", nil},
		{"not base64", "plugins", "", present("%%%not base64%%%"), "", nil},
		{"15 bytes", "plugins", "", present(b64(testFloodgateKey[:15])), "", nil},
		{"17 bytes", "plugins", "", present(b64(append([]byte("x"), testFloodgateKey...))), "", nil},
		{"empty value", "plugins", "", present(""), "", nil},
		{"no plugins or mods dir", "", "", present(b64(testFloodgateKey)), "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.layout != "" {
				if err := os.MkdirAll(filepath.Join(dir, tc.layout), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.existing != "" {
				p := filepath.Join(dir, filepath.FromSlash(tc.existing))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("floodgate generated its own"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			applyFloodgateKey(dir, tc.fetch)

			for _, rel := range []string{"plugins/floodgate/key.pem", "config/floodgate/key.pem"} {
				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
				if rel == tc.wantFile {
					if err != nil || !bytes.Equal(got, tc.wantKey) {
						t.Errorf("%s = %q, %v; want %q", rel, got, err, tc.wantKey)
					}
					continue
				}
				if err == nil && tc.existing != rel {
					t.Errorf("%s was written: %q", rel, got)
				}
			}
			if tc.wantFile == "" && tc.existing != "" {
				got, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tc.existing)))
				if string(got) != "floodgate generated its own" {
					t.Errorf("a refused value changed the existing key: %q", got)
				}
			}
		})
	}
}

func TestWriteFloodgateKeyRefusesEscapingLink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "plugins", "floodgate")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := writeFloodgateKey(dir, testFloodgateKey); err == nil {
		t.Error("write through a link that leaves the server directory was not refused")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the link target outside the server directory was written to: %v", entries)
	}
}
