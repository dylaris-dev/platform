package corestorage

import (
	"encoding/json"
	"testing"
)

// The modpack-storage answer moved from AppState onto the Manager with its
// cache (S12). Pin both halves of the cache contract: a second ask inside the
// TTL does not rebuild the provider (it is behind GET /api/system/features,
// which every user hits), and InvalidateModpack - called by the writes that
// can change the answer - makes the next ask see the change at once.
func TestModpackConfiguredIsCachedUntilInvalidated(t *testing.T) {
	fs := &coreStorageFakeStore{values: map[string]string{}}
	m := &Manager{Store: fs}

	if m.ModpackConfigured() {
		t.Fatal("no modpack storage settings, yet configured")
	}

	paths, _ := json.Marshal([]string{t.TempDir()})
	fs.values["modpack_storage_provider"] = "local"
	fs.values["modpack_storage_paths"] = string(paths)
	if m.ModpackConfigured() {
		t.Fatal("the answer changed inside the TTL without an invalidation; the cache is not being used")
	}

	m.InvalidateModpack()
	if !m.ModpackConfigured() {
		t.Fatal("after InvalidateModpack the configured storage is still reported missing")
	}
}
