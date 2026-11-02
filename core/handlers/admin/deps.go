package admin

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Deps is what the database migration and CPU pinning handlers read. Same
// field names as handlers.AppState; routes.go fills it once. A nil field keeps
// the meaning it had on AppState.
type Deps struct {
	Store        store.Store
	Authz        *authz.Resolver
	FeatureFlags *services.FeatureFlags
	Events       *services.SystemEventsPublisher
	DBMigration  *services.DBMigrationService
	CPUPinning   *services.CPUPinningService
	DBType       string
}
