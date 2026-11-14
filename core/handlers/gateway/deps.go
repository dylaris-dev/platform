package gateway

import (
	"database/sql"
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Deps is what the gateway, warp, custom-domain, DNS, bandwidth and voice
// handlers read. Same field names as handlers.AppState; routes.go fills it
// once. Gateway and Admission keep their "nil = not deployed / no gate"
// meaning: the values are copied as they are on AppState, so a nil stays nil.
type Deps struct {
	Store        store.Store
	Redis        *redis.Client
	FeatureFlags *services.FeatureFlags
	Gateway      services.GatewayProvider

	// GatewayRemote is the gateway provider mode; nil is local. RemoteGateway
	// is the managed-gateway provider used while it is remote.
	GatewayRemote func() (remote, known bool)
	RemoteGateway *services.RemoteGateway
	// GatewayProviderConfig resolves the provider settings, env over settings.
	GatewayProviderConfig func() (services.ProviderConfig, error)

	GRPCRegistry *nodegrpc.Registry
	Admission    *services.AdmissionGate

	SuspendGrace       time.Duration
	StoreEnabled       bool
	StoreURL           string
	StoreSharedKey     string
	ClusterSecret      string
	GatewayHubURL      string
	GRPCTLSEnabled     bool
	GRPCTLSFingerprint string

	// LongTermRead is the long-term statistics pool, asked per request (an
	// operator switches recording on and off at runtime); nil = not recording.
	// A func rather than *metrics.Manager: handler packages do not import
	// metrics (layering_test).
	LongTermRead func() *sql.DB

	// Authz answers what the CALLER holds inside a handler, where one answer
	// carries blocks guarded by different capabilities; nil holds nothing.
	Authz *authz.Resolver
}
