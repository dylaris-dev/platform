package console

import (
	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the console, players and scheduled task handlers read. Same
// field names as handlers.AppState; routes.go fills it once. A nil field keeps
// the meaning it had on AppState (a nil ConsoleRedis falls back to Redis).
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	ConsoleRedis *redis.Client
	Authz        *authz.Resolver
	Events       *services.SystemEventsPublisher
	GRPCRegistry *nodegrpc.Registry
	StoreEnabled bool
}
