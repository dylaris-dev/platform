package settings

import (
	"dylaris-core/authz"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the settings, feature, auth/audit policy, mail template,
// maintenance, module, panel role, permissions mode, plan, region and timezone
// handlers read. Same field names as handlers.AppState; routes.go fills it
// once. A nil field keeps the meaning it had on AppState.
type Deps struct {
	Store            store.Store
	Redis            *redis.Client
	Events           *services.SystemEventsPublisher
	FeatureFlags     *services.FeatureFlags
	Cache            *services.Cache
	Authz            *authz.Resolver
	CoreStorage      *corestorage.Manager
	RoutingMigration *services.RoutingMigrationService
	FrontendURL      string
	// GatewayRemote is the gateway provider mode; nil is local.
	GatewayRemote func() (remote, known bool)
	// RemoteGateway is the managed-gateway provider used while it is remote.
	RemoteGateway *services.RemoteGateway
	// ResetGatewayRemote drops GatewayRemote's cache; nil is a no-op.
	ResetGatewayRemote func()
	// GatewayProviderConfig resolves the provider settings, env over settings.
	GatewayProviderConfig func() (services.ProviderConfig, error)
}
