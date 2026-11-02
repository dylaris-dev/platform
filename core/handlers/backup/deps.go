package backup

import (
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the server-backup and platform-backup handlers read. Same field
// names as handlers.AppState; routes.go fills it once (main.go's scheduler
// shares the routes.go handler). An empty PlatformDB.Name still means "no
// platform database to dump".
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	Queue        *services.QueueService
	GRPCRegistry *nodegrpc.Registry
	FeatureFlags *services.FeatureFlags
	Authz        *authz.Resolver
	CoreStorage  *corestorage.Manager

	PlatformDB            services.PGConn
	PlatformDBMajor       int
	PlatformBackupWorkDir string

	SuspendGrace   time.Duration
	StoreEnabled   bool
	ReleaseVersion string
	ClusterSecret  string
}
