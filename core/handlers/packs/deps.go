package packs

import (
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Deps is what the pack handlers read. Same field names as
// handlers.AppState; routes.go fills it once.
type Deps struct {
	Store         store.Store
	Events        *services.SystemEventsPublisher
	Cache         *services.Cache
	FeatureFlags  *services.FeatureFlags
	CoreStorage   *corestorage.Manager
	ClusterSecret string
}
