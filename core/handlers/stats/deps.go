package stats

import (
	"dylaris-core/metrics"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the stats and metrics handlers read. Same field names as
// handlers.AppState; routes.go fills it once. A nil field keeps the meaning it
// had on AppState.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	PubSub       *services.PubSubHub
	Events       *services.SystemEventsPublisher
	FeatureFlags *services.FeatureFlags
	Metrics      *metrics.Manager
}
