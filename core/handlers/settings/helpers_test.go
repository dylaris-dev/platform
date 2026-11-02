package settings

import "dylaris-core/services/corestorage"

// withCoreStorage gives a test Deps the core storage the handlers reached
// through AppState before S12: a Manager over the very same objects.
func withCoreStorage(d Deps) Deps {
	d.CoreStorage = &corestorage.Manager{Store: d.Store, Redis: d.Redis, Events: d.Events}
	return d
}
