package servers

import (
	"context"
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the server handlers (lifecycle, mods, members, roles, grants,
// placement, ports, rcon, technic) read. Same field names as
// handlers.AppState; routes.go fills it once. The pointers and interfaces that
// mean "feature off" when nil (Gateway, Migration, CPUPinning, NetPolicy,
// Cache) are copied as they are on AppState, so a nil stays nil.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Queue        *services.QueueService
	GRPCRegistry *nodegrpc.Registry
	Events       *services.SystemEventsPublisher
	FeatureFlags *services.FeatureFlags
	Authz        *authz.Resolver
	Cache        *services.Cache
	Gateway      services.GatewayProvider
	CoreStorage  *corestorage.Manager
	Migration    *services.MigrationOrchestrator
	CPUPinning   *services.CPUPinningService
	NetPolicy    interface{ RunOnce(ctx context.Context) }

	SuspendGrace   time.Duration
	StoreEnabled   bool
	Hosted         bool
	ReleaseVersion string

	// InstallMrpackURL is wired in routes.go to the packs handler (assigned on
	// AppState inside buildAPIRouter, so serversDeps is built after it).
	InstallMrpackURL func(ctx context.Context, pack *models.Pack, build *models.PackBuild) (string, error)

	// GatewayRemote is the gateway provider mode; nil is local. RemoteGateway
	// is the managed-gateway provider used while it is remote.
	GatewayRemote func() (remote, known bool)
	RemoteGateway *services.RemoteGateway
}
