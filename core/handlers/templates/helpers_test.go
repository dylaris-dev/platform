package templates

import (
	"dylaris-core/handlers"
	"dylaris-core/services/corestorage"
)

// withCoreStorage gives a test state the core storage it implicitly was before
// S12 (the AppState methods ran over its own Store, Redis, Events, ...): a
// Manager over the very same objects.
func withCoreStorage(s *handlers.AppState) *handlers.AppState {
	s.CoreStorage = &corestorage.Manager{Store: s.Store, Redis: s.Redis, Events: s.Events, ClusterSecret: s.ClusterSecret, Registry: s.GRPCRegistry}
	return s
}
