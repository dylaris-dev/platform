package backup

import (
	"dylaris-core/services/corestorage"
)

// testDeps is routes.go's backupDeps for tests: a core storage Manager over
// the same objects, as the AppState methods ran over them before S12.
func testDeps(d Deps) Deps {
	d.CoreStorage = &corestorage.Manager{Store: d.Store, Redis: d.Redis, ClusterSecret: d.ClusterSecret, Registry: d.GRPCRegistry}
	return d
}
