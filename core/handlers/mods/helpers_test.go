package mods

import "dylaris-core/store"

// Copies of handlers test fixtures the mods tests use; each keeps its name.

// coreStorageHTTPFakeStore is a read/write settings fake for the CRUD +
// test-connection handler tests below. Distinct from coreStorageFakeStore
// (core_storage_builder_test.go, GetSetting-only, read-side tests) and from
// modeFakeStore (permissions_mode_test.go, always returns one fixed value):
// this one records SetSetting writes into a map so a save-then-get round
// trip can be asserted.
type coreStorageHTTPFakeStore struct {
	store.Store
	kv map[string]string
}

func newCoreStorageHTTPFakeStore() *coreStorageHTTPFakeStore {
	return &coreStorageHTTPFakeStore{kv: map[string]string{}}
}

func (f *coreStorageHTTPFakeStore) GetSetting(key string) (string, error) { return f.kv[key], nil }

// SetSettingBy is the audited write path; the fake records the value and drops
// the actor, so a handler that writes through it fails its assertion rather than
// panicking on a nil embedded Store.
func (f *coreStorageHTTPFakeStore) SetSettingBy(key, value, _ string) error {
	return f.SetSetting(key, value)
}

func (f *coreStorageHTTPFakeStore) SetSetting(key, value string) error {
	f.kv[key] = value
	return nil
}
