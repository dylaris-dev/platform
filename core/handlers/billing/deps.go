package billing

import (
	"context"
	"log"
	"time"

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
	// SuspendGrace is the cutoff grace the beam issuer publisher applies, the
	// same one the gateway handlers read.
	SuspendGrace time.Duration
}

// publishBeamIssuers republishes the owner's beam issuer records after a
// billing change. Logged, not answered: the reconcile loop heals within five
// minutes, and a provision must not fail over a Redis blip.
func (d Deps) publishBeamIssuers(ctx context.Context, ownerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := services.PublishBeamIssuers(ctx, d.Store, d.Redis, d.SuspendGrace, ownerID); err != nil {
		log.Printf("beam issuers %s: publish after billing change failed, the reconcile loop retries: %v", ownerID, err)
	}
}
