package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A .mrpack manifest is attacker-controlled in practice: the host allowlist
// admits github.com and raw.githubusercontent.com, so anyone who can reach the
// setup endpoint can point the node at a pack they wrote. Every number in that
// manifest is therefore a claim, and the caps have to hold when the claim is a
// lie.
func TestModpackFileCapNeverTrustsTheManifest(t *testing.T) {
	cases := []struct {
		name     string
		declared int64
		want     int64
	}{
		// The one that was exploitable: fileSize+slack came out <= 0, which
		// selected the 4 GB WHOLE-PACK cap as the limit for a single file.
		{"large negative", -100000, maxModpackFile},
		{"small negative", -1, maxModpackFile},
		{"zero / absent", 0, maxModpackFile},
		{"absurdly large", 8 << 30, maxModpackFile},
		{"exactly at the ceiling", maxModpackFile - modpackSizeSlack, maxModpackFile},
		{"honest size", 10 << 20, (10 << 20) + modpackSizeSlack},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := modpackFileCap(c.declared)
			if got != c.want {
				t.Errorf("modpackFileCap(%d) = %d, want %d", c.declared, got, c.want)
			}
			if got > maxModpackFile {
				t.Errorf("modpackFileCap(%d) = %d, which is above the per-file ceiling %d; "+
					"no manifest value may raise this cap", c.declared, got, maxModpackFile)
			}
		})
	}
}

// The cap has to fail the download, not silently cut it short. A truncated file
// that reports success is worse than a failed one: with no sha512 in the
// manifest it gets renamed into place and the server crash-loops on a corrupt
// jar, with nothing pointing at the size.
func TestDownloadFileBoundedRefusesAnOversizedBody(t *testing.T) {
	const cap = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, cap*4))
	}))
	defer srv.Close()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	n, err := downloadBoundedInto(root, "oversized.jar", srv.URL, cap)
	if err == nil {
		t.Fatalf("a %d-byte body under a %d-byte cap was accepted (wrote %d bytes)", cap*4, cap, n)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "oversized.jar")); !os.IsNotExist(statErr) {
		t.Errorf("the rejected download was left on disk")
	}
}

// A body at exactly the cap is legitimate and must still land.
func TestDownloadFileBoundedAcceptsExactlyTheCap(t *testing.T) {
	const cap = 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, cap))
	}))
	defer srv.Close()

	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	n, err := downloadBoundedInto(root, "exact.jar", srv.URL, cap)
	if err != nil {
		t.Fatalf("a body of exactly the cap was rejected: %v", err)
	}
	if n != cap {
		t.Errorf("reported %d bytes written, want %d", n, cap)
	}
	info, err := os.Stat(filepath.Join(dir, "exact.jar"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != cap {
		t.Errorf("file is %d bytes on disk, want %d", info.Size(), cap)
	}
}
