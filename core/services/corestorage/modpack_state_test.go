package corestorage

import (
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

// With two Core replicas, the save lands on one of them and the panel's
// refresh on modpack_settings.changed can land on the other. The other
// replica's cached answer must not outlive the save (C64).
func TestModpackConfiguredInvalidationReachesOtherReplica(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &coreStorageFakeStore{values: map[string]string{}}
	saver := &Manager{Store: fs, Redis: rdb}
	other := &Manager{Store: fs, Redis: rdb}

	if other.ModpackConfigured() {
		t.Fatal("no modpack storage settings, yet configured")
	}
	paths, _ := json.Marshal([]string{t.TempDir()})
	fs.values["modpack_storage_provider"] = "local"
	fs.values["modpack_storage_paths"] = string(paths)
	saver.InvalidateModpack()

	if !other.ModpackConfigured() {
		t.Fatal("the other replica still answers its cached 'not configured' after the save")
	}
	// Unchanged generation: the other replica keeps using its cache.
	fs.values["modpack_storage_provider"] = "s3" // no bucket: would not resolve
	if !other.ModpackConfigured() {
		t.Fatal("answer changed without an invalidation; the cache is not being used")
	}
}
