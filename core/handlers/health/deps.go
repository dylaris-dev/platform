package health

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the status, overview, updates, system, versions, DNS and
// notification handlers read. Same field names as handlers.AppState; routes.go
// fills it once. A nil field keeps the meaning it had on AppState.
type Deps struct {
	Store            store.Store
	Redis            *redis.Client
	PubSub           *services.PubSubHub
	Authz            *authz.Resolver
	FeatureFlags     *services.FeatureFlags
	CoreStorage      *corestorage.Manager
	RoutingMigration *services.RoutingMigrationService
	DBType           string
	FrontendURL      string
	PanelAPIURL      string
	ClusterSecret    string
	StoreEnabled     bool
	Hosted           bool
	StoreURL         string
	StoreSharedKey   string
	ReleaseVersion   string
	CoreID           string
	// GatewayRemote is the gateway provider mode; nil is local.
	GatewayRemote func() (remote, known bool)
	// RemoteGateway is the managed-gateway provider used while it is remote.
	RemoteGateway *services.RemoteGateway
}
