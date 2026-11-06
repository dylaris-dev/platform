package nodes

import (
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the node, enroll and admission handlers read. Same field names
// as handlers.AppState; routes.go fills it once. Gateway and WarpPeers keep
// their "nil = not deployed" meaning: the interface values are copied as they
// are on AppState, so a nil stays nil.
type Deps struct {
	Store              store.Store
	Redis              *redis.Client
	FeatureFlags       *services.FeatureFlags
	GRPCRegistry       *nodegrpc.Registry
	Queue              *services.QueueService
	Gateway            services.GatewayProvider
	WarpPeers          services.WarpPeerDisconnector
	CoreStorage        *corestorage.Manager
	StoreEnabled       bool
	StoreURL           string
	StoreSharedKey     string
	GRPCTLSEnabled     bool
	GRPCTLSFingerprint string
	ClusterSecret      string
}
