package templates

import (
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"
)

// Deps is what the templates handlers read. Same field names as
// handlers.AppState; routes.go fills it once. A nil GRPCRegistry refuses a
// capture (no node to read from), as before.
type Deps struct {
	Store        store.Store
	Authz        *authz.Resolver
	FeatureFlags *services.FeatureFlags
	GRPCRegistry *nodegrpc.Registry
	CoreStorage  *corestorage.Manager
	SuspendGrace time.Duration
}
