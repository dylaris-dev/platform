package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An empty UUID must never resolve to a real directory.
//
// GetServerPath's filesystem scan asks whether filepath.Join(p, uuid) is a
// directory, and Join(p, "") is p - so every storage path matched, the resolver
// returned the storage ROOT, and GetServerDir handed that root back verbatim.
// The delete command feeds exactly that value to os.RemoveAll: one delete with
// an unnamed target would have erased every server on the node, of every
// tenant. delete_sub_server had always guarded its equivalent empty name.
func TestEmptyUUIDNeverResolvesToAStoragePath(t *testing.T) {
	sm, _ := newPlacementManager(t, 2)
	paths := sm.Paths()

	// "." resolved to the storage root too, and ".." above it; the root holds
	// .node_secret.
	for _, uuid := range []string{"", " ", "\t", ".", "..", "../..", "srv/../.."} {
		if got := sm.GetServerPath(uuid); got != "" {
			t.Errorf("GetServerPath(%q) = %q, want \"\"; that path holds every server on the node", uuid, got)
		}
		if got := sm.GetServerDir(uuid); got != "" {
			t.Errorf("GetServerDir(%q) = %q, want \"\"; os.RemoveAll on it wipes the node", uuid, got)
		}
	}

	// A real server still resolves, including via the filesystem-scan branch
	// that the empty case abused (nil Redis here, so the scan is what runs).
	const uuid = "srv-uuid"
	want := filepath.Join(paths[1], uuid)
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := sm.GetServerDir(uuid); got != want {
		t.Errorf("GetServerDir(%q) = %q, want %q - ordinary resolution must be unaffected", uuid, got, want)
	}
}
