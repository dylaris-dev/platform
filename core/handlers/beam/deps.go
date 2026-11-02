package beam

import (
	"time"

	"dylaris-core/authz"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the beam handlers read. Same field names as handlers.AppState;
// routes.go fills it once. A nil Authz refuses every beam session.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Authz        *authz.Resolver
	StoreEnabled bool
	SuspendGrace time.Duration
}
