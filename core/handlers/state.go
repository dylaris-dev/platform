package handlers

import (
	"context"
	"dylaris-core/metrics"
	"sync"
	"time"

	"dylaris-core/authz"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

type AppState struct {
	// Metrics owns the long-term statistics recorder and can swap its database
	// while Core runs, which is what lets the target be a panel setting rather
	// than a restart. Ask it for a Handle rather than holding one; nil, and a
	// nil Handle from it, are both supported states - every reader must cope,
	// because a statistics store is never a reason for anything else to fail.
	Metrics *metrics.Manager

	Store            store.Store
	Redis            *redis.Client
	Queue            *services.QueueService
	GRPCRegistry     *nodegrpc.Registry
	Gateway          services.GatewayProvider
	RoutingMigration *services.RoutingMigrationService
	// Admission evaluates the node-admission NETWORK gate on the warp enrol,
	// which is the only place that sees a BYON customer's real IP (the gRPC
	// node-enrol arrives through the tunnel, so its peer is the warp leader).
	// nil = no admission configured, and the gate is skipped.
	Admission *services.AdmissionGate
	// FrontendURL is the panel base URL — used by mailers to build absolute
	// links into the UI (verify-email, password-reset, ticket replies, etc).
	FrontendURL string

	// PanelAPIURL is PANEL_API_URL: the API base Core renders into every page,
	// and EMPTY for the normal deployment where the panel calls /api on its own
	// origin. Read by the DNS check, which has to tell "the API is on this
	// page's own domain" apart from "the operator put it on a second hostname".
	PanelAPIURL string

	// ExternalTicketDBURL carries the operator-configured connection
	// string for the external ticket DB. Empty when not configured;
	// the migration handler reads this to drive the UI.
	ExternalTicketDBURL string

	// Events publishes platform-wide config-change events into Redis
	// Pub/Sub so connected panels refresh without polling. Nil-safe:
	// publishers handle a nil pointer / nil Redis gracefully.
	Events *services.SystemEventsPublisher

	// Cache is where cached third-party metadata (the Modrinth proxy, the
	// cross-version availability check) is kept. It points at Redis by default
	// and at a dedicated endpoint when the operator configures one; see
	// mod_cache_settings.go for why that is optional rather than a gate.
	Cache *services.Cache

	// FeatureFlags is the cached platform feature toggle reader.
	// Read by the modpack route gates; flipped via /admin/settings/modpacks.
	FeatureFlags *services.FeatureFlags

	// Authz is the capability resolver + RequireCap middleware factory (the
	// unified permission-system chokepoint). Constructed at boot; NOT yet wired
	// into the ~400 routes (phase 2). Only GET /api/authz/catalog exists today.
	Authz *authz.Resolver

	// Migration is the leader-driven node-to-node migration orchestrator.
	// The manual-move endpoint only ENQUEUES onto it; the elected Core executes.
	Migration *services.MigrationOrchestrator

	// DBMigration drives the in-panel cross-database migration (copy the whole
	// DB onto a new target under maintenance mode). Shared job state lives in
	// Redis so every admin sees the same live status.
	DBMigration *services.DBMigrationService

	// StorageMigration drives the in-panel blob storage migration (inventory,
	// copy, verify, optionally delete the source) for all five blob data
	// sets. Shared job state lives in Redis so every admin sees the same live
	// status. main.go assigns this unconditionally, so the nil branches the
	// handlers keep are reachable only from tests that construct an AppState
	// directly.
	StorageMigration *services.StorageMigrationService

	// CPUPinning reads node CPU topology (published to Redis by each node) and
	// computes auto cpusets for per-server CPU pinning.
	CPUPinning *services.CPUPinningService

	// StorageGate is the watchdog for the configured host-path core storage.
	// SyncStorageGate points it at the current path (or stops it when the
	// backend is s3), and every per-request provider build consults it. Every
	// Gate method is nil-safe, so an AppState built without one - which is
	// only ever a test - simply never gates.
	StorageGate *storage.Gate

	// StorageS3 is the connection state and retry loop for the configured s3
	// core storage, the S3-side counterpart to StorageGate. It is long-lived
	// because providers are rebuilt per request, so state kept on a provider
	// would reset before anything could read it. Every method is nil-safe, so
	// an AppState built without one - only ever a test - simply never pauses.
	StorageS3 *storage.S3Resilience

	// PlatformDB is how to reach Core's OWN database with an external tool.
	// Not a handle - the handle lives in the store - but the connection
	// DESCRIPTION pg_dump and pg_restore need as a subprocess, for platform
	// backups.
	PlatformDB services.PGConn
	// PlatformDBMajor is that server's major version, asked once at boot.
	//
	// Asked rather than configured, and carried rather than re-asked: it
	// decides WHICH installed client is run, and the constraint points both
	// ways - pg_dump refuses a newer server, pg_restore emits SQL an older one
	// does not understand.
	PlatformDBMajor int
	// PlatformBackupWorkDir is where a platform run spools components before
	// they are sealed. A component of unknown length - a dump, a storage walk -
	// has to land on disk first, because tar writes a member's size ahead of
	// its bytes.
	PlatformBackupWorkDir string

	// StorageStatus forwards StorageGate's and StorageS3's transitions onto the
	// system-events channel and answers GET /api/storage/connection. It reads
	// both backends live rather than mirroring them. Nil-safe, so an AppState
	// built without one - only ever a test - simply reports both backends ok.
	StorageStatus *services.StorageStatus

	// StorageReach proves this Core can actually reach the SHARED core file
	// storage, and gates this Core's storage routes when it cannot. Nil in
	// tooling and tests, which the gate treats as "not enabled" rather than
	// "deny".
	StorageReach *storagereach.Service

	// reachRoundDeadline overrides the config-round cap. Zero means the
	// default; it exists so tests do not wait 15s per case.
	reachRoundDeadline time.Duration

	// reachRoundPoll overrides the round's poll cadence, which is also how
	// often the coordinator retries its OWN storage listing. Zero means the
	// production default (storagereach's 1s). It exists because that cadence
	// decides how many chances the coordinator gets to observe a peer's beacon
	// before the deadline: at 1s a short test deadline buys one or two listings
	// and the positive-control test then depends on a peer goroutine being
	// scheduled inside a single second, which held locally and flaked on CI.
	// Production keeps 1s - a config save is interactive, and polling shared
	// storage faster than that buys nothing.
	reachRoundPoll time.Duration

	// reachRoundMu is the single-process half of the config-round lock that
	// checkSharedStorageReachable takes before calling RunRound: the Redis
	// SETNX lock alongside it is the cluster-wide half. Zero value is a ready
	// to use, unlocked mutex, so no constructor change is needed.
	reachRoundMu sync.Mutex

	// Billing drives the BYON non-payment lifecycle (past_due grace -> suspended
	// -> retention cleanup). Handlers call EnterPastDue/Reactivate/Suspend; the
	// leader-gated worker progresses expired grace windows. Nil-safe in callers.
	Billing *services.BillingLifecycleService

	// DBType is the normalized database backend ("timescaledb" or "postgres"),
	// set from config at boot. The status page uses it to distinguish an
	// intended plain-postgres deployment from a misconfigured timescale one.
	DBType string

	// StoreEnabled mirrors config.StoreEnabled: true only when BOTH StoreURL
	// and StoreSharedKey are set. Gates every store-coupled surface — the
	// connect-store button, store-linking endpoints, and the demo account/
	// servers feature — so a self-hosted open-core build with no store ENV
	// exposes none of it.
	StoreEnabled bool
	// StoreURL is the dylaris.com storefront base URL (e.g. https://dylaris.com).
	// Used to build the connect-store redirect and to read link status.
	StoreURL string
	// StoreSharedKey is the service-to-service trust shared with dylaris.com. It
	// authenticates store->core calls (link/verify, verify-user) and core->store
	// calls (link-status). NOT a user proof.
	StoreSharedKey string

	// GRPCTLSFingerprint is the cluster-wide Core control-channel cert fingerprint
	// (public pinning material). Surfaced to BYON operators so a secret-free node
	// can pin it via GRPC_TLS_FINGERPRINT. Empty string if derivation failed.
	GRPCTLSFingerprint string
	// GRPCTLSEnabled mirrors config.GRPCTLSEnabled: whether the node<->Core gRPC
	// control channel actually enforces TLS + fingerprint pinning. Gates whether
	// GRPCTLSFingerprint is worth handing to an operator (see node_enroll.go).
	GRPCTLSEnabled bool

	// ACLProvisioner provisions the route-only links' scoped Redis ACL users on
	// Core's own Redis client (link-boot + revoke + billing suspend/reactivate).
	ACLProvisioner *redisacl.Provisioner
	// ClusterSecret is the deployment-wide secret used to derive per-link tunnel
	// tokens and Redis passwords. Never sent to a tenant.
	ClusterSecret string

	// GatewayHubURL is the gateway Hub's internal base URL. Empty when no
	// gateway is deployed, which the DNS settings surface reports rather than
	// failing: a platform-only install has no records to write.
	GatewayHubURL string

	// NetPolicy republishes the per-server ingress policy. Held so linking a
	// proxy takes effect at once instead of on the next tick - a person who just
	// pressed the button is watching for it to work.
	NetPolicy interface{ RunOnce(ctx context.Context) }

	// AdminSecret mirrors config.AdminSecret: the RAM-only break-glass secret
	// that gates /setup admin creation. Empty = feature disabled. Never
	// persisted, never logged.
	AdminSecret string

	// SetupEnabled mirrors config.SetupEnabled (env SETUP): whether /setup stays
	// reachable once an admin exists. Ignored while no admin exists. See the
	// config field for why the default is false.
	SetupEnabled bool

	// SuspendGrace mirrors config.SuspendGrace: how long after a tenant is marked
	// "suspended" the LinkBoot gate keeps letting their route-only links boot. The
	// gate uses it via linkHardSuspended; MUST match the reconciler + enforcement
	// cutoff so a link is refused exactly when its ACL is actually gone.
	SuspendGrace time.Duration

	// TabProxyHostSuffix mirrors config.TabProxyHostSuffix: the DNS suffix each
	// proxied tab is served under, one host per tab. Empty means proxied tabs
	// are not available at all - there is nowhere safe to serve them.
	TabProxyHostSuffix string

	// UpdatesURLPlatform / UpdatesURLHosted mirror config: the public raw URLs of
	// the two release-notes files. Empty means "use the embedded copy only".
	UpdatesURLPlatform string
	UpdatesURLHosted   string

	// ReleaseVersion is the release THIS Core image was built from, stamped in at
	// build time. Empty on a local build, which reads as "not reporting".
	//
	// Deliberately not derived from the embedded platform.md: the repo has one
	// version and more than one audience, so a release that only concerns
	// customers leaves platform.md's top block behind and Core would report
	// itself behind for a release it already contains.
	ReleaseVersion string

	// CoreID is DYLARIS_CORE_ID, this instance's own name in the heartbeat
	// keyspace. The updates view needs it to tell its own row apart from a
	// sibling Core's.
	CoreID string

	// modpackStorage caches "does modpack storage resolve to a provider", which
	// GET /api/system/features reports to every user at boot. See
	// modpack_storage_state.go for why that call must not run per request.
	modpackStorage modpackStorageState
}

// AdminSecretConfigured reports whether the break-glass ADMIN_SECRET is set.
func (s *AppState) AdminSecretConfigured() bool { return s.AdminSecret != "" }
