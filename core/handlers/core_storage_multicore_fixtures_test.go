package handlers

import (
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
)

// Copies of storageapi/core_storage_multicore_test.go fixtures the core storage
// reach tests here still use.

// multiCoreRedis returns a client whose keyspace holds exactly the heartbeats
// named by ids, written the way CoreHeartbeatService writes them. No ids means
// a live Redis with an empty keyspace; use downRedis for the unreachable case.
func multiCoreRedis(t *testing.T, ids ...string) *redis.Client {
	t.Helper()
	rdb, _ := multiCoreRedisServer(t, ids...)
	return rdb
}

// sharedLocalFactory hands every participant a LocalProvider on the same
// root, mirroring services/storagereach/round_test.go's sharedFactory: what a
// genuinely shared mount looks like from the verifier's point of view. It
// ignores cfg entirely (backend, path, s3 fields) on purpose - the tests below
// are about SaveConfig's wiring of the round, not about proving a real
// filesystem or S3 endpoint, exactly like round_test.go's own use of it with
// an s3-flavoured Config.
func sharedLocalFactory(root string) storagereach.ProviderFactory {
	return func(storagereach.Config) (storage.StorageProvider, error) {
		return &storage.LocalProvider{BasePath: root}, nil
	}
}

// multiCoreRedisServer is multiCoreRedis plus the server behind it, for the one
// test that has to inject a per-command fault (SetPreHook) rather than just
// seed keys.
func multiCoreRedisServer(t *testing.T, ids ...string) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	for _, id := range ids {
		data, err := json.Marshal(services.CoreHeartbeat{ID: id})
		if err != nil {
			t.Fatalf("marshal heartbeat: %v", err)
		}
		mr.Set("dylaris:core:"+id, string(data))
	}
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}
