package storageapi

import (
	"sync/atomic"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/models"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Copies of handlers test fixtures the storage tests use; each keeps its name.

func seedCoreStorageS3(fs *handlertest.SettingsStore) {
	fs.KV[corestorage.KeyBackend] = "s3"
	fs.KV[corestorage.KeyS3Endpoint] = "https://s3.example.com"
	fs.KV[corestorage.KeyS3Bucket] = "bucket1"
	fs.KV[corestorage.KeyS3AccessKey] = "key1"
	fs.KV[corestorage.KeyS3SecretKey] = "stored-secret"
}

// resolveFakeStore serves settings + one storage connection. Embeds store.Store
// so only the two methods the resolver touches need bodies.
type resolveFakeStore struct {
	store.Store
	settings map[string]string
	conn     *models.StorageConnection
	connErr  error
	askedID  int
}

func (f *resolveFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }

func (f *resolveFakeStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	f.askedID = id
	if f.connErr != nil {
		return nil, f.connErr
	}
	return f.conn, nil
}

// coreStorageFakeStore returns a per-key canned setting value from an
// internal map. This is distinct from modeFakeStore (permissions_mode_test.go),
// which always returns the same value regardless of the requested key and so
// cannot drive the multi-key CoreStorageConfig shape.
type coreStorageFakeStore struct {
	store.Store
	values map[string]string
}

// GetSetting mirrors the real store's behavior for an unset setting: an empty
// value and a nil error.
func (f *coreStorageFakeStore) GetSetting(key string) (string, error) {
	return f.values[key], nil
}

// coreStorageTargetFakeStore is a settings-only store for this file. Local
// here; never redeclared elsewhere.
type coreStorageTargetFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageTargetFakeStore() *coreStorageTargetFakeStore {
	return &coreStorageTargetFakeStore{kv: map[string]string{}}
}

func (f *coreStorageTargetFakeStore) GetSetting(k string) (string, error) { return f.kv[k], nil }

func (f *coreStorageTargetFakeStore) SetSetting(k, v string) error { f.kv[k] = v; return nil }

// storageReachDepsCounters counts every call into the three ServiceDeps hooks
// that SelfCheck and RunRound touch, so the test below can assert an
// INVOCATION count instead of an outcome.
//
// An outcome-based check (before/after status, or "no round key landed in
// Redis") can be fooled two different ways: an unconfigured backend makes
// SelfCheck produce the exact same idempotent ok result every time (so a
// repeat call is invisible in a status diff), and checkSharedStorageReachable
// short-circuits on "not more than one online Core" before it ever reaches
// the round machinery (so a wrongly-wired round call leaves no round key
// either). A call count sidesteps both: ConfigFor is the first thing observe
// does inside SelfCheck regardless of what it returns, and NewProvider is
// what RunRound's Coordinator calls for ITS OWN participation regardless of
// how many peers there are - so both fire on the very first line of the code
// path they claim to guard, before either masking condition gets a chance to
// apply.
type storageReachDepsCounters struct {
	configFor   atomic.Int64
	onlineCores atomic.Int64
	newProvider atomic.Int64
}

// testDeps is routes.go's storageapiDeps for tests: a core storage Manager over
// the same objects, as the AppState methods ran over them before S12.
func testDeps(d Deps) Deps {
	d.CoreStorage = &corestorage.Manager{Store: d.Store, Redis: d.Redis, Events: d.Events, Registry: d.GRPCRegistry}
	return d
}
