package billing

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the billing, store, usage and traffic-limit handlers read.
// Same field names as handlers.AppState; routes.go fills it once.
type Deps struct {
	Store          store.Store
	Redis          *redis.Client
	Authz          *authz.Resolver
	Billing        *services.BillingLifecycleService
	FrontendURL    string
	StoreEnabled   bool
	StoreURL       string
	StoreSharedKey string
}
