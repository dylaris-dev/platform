package tickets

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the ticket handlers read. Same field names as
// handlers.AppState; routes.go fills it once.
type Deps struct {
	Store               store.Store
	Redis               *redis.Client
	Authz               *authz.Resolver
	Events              *services.SystemEventsPublisher
	CoreStorage         *corestorage.Manager
	ExternalTicketDBURL string
}
