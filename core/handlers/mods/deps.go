package mods

import (
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Deps is what the mod and modpack-settings handlers read. Same field names as
// handlers.AppState; routes.go fills it once. A nil Cache means no response
// caching, as before.
type Deps struct {
	Store       store.Store
	Cache       *services.Cache
	Events      *services.SystemEventsPublisher
	CoreStorage *corestorage.Manager
}
