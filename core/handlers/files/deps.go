package files

import (
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the file manager reads. Same field names as handlers.AppState;
// routes.go fills it once. A nil field keeps the meaning it had on AppState.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Authz        *authz.Resolver
	GRPCRegistry *nodegrpc.Registry
	StoreEnabled bool
	SuspendGrace time.Duration
}
