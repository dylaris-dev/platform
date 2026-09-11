package main

// ci: trigger full pipeline run (no-op, safe to remove)

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dylaris-core/authz"
	"dylaris-core/config"
	"dylaris-core/database"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/handlers"
	"dylaris-core/metrics"
	"dylaris-core/models"
	"dylaris-core/panelfs"
	"dylaris-core/pkg/leader"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
	"dylaris-core/store"
	beamauth "dylaris-pkg/beam/auth"
	"dylaris-pkg/errlog"

	gorillaHandlers "github.com/gorilla/handlers"
)

// detectBeamPlatform maps a browser User-Agent header to one of the platform
// slugs in handlers.validBeamPlatforms, which is what /api/beam/download
// resolves against the signed release manifest. (It used to name the
// beam-relay's /download/{slug}; the relay stopped serving binaries in
// eeff445.) Conservative: anything unrecognised falls back to linux-amd64.
func detectBeamPlatform(ua string) string {
	u := strings.ToLower(ua)
	switch {
	case strings.Contains(u, "windows"):
		return "windows-amd64"
	case strings.Contains(u, "mac os x"), strings.Contains(u, "macintosh"):
		if strings.Contains(u, "arm") {
			return "darwin-arm64"
		}
		return "darwin-amd64"
	case strings.Contains(u, "linux"):
		if strings.Contains(u, "aarch64") || strings.Contains(u, "arm64") {
			return "linux-arm64"
		}
		return "linux-amd64"
	default:
		return "linux-amd64"
	}
}

// beamToolsRedirect serves GET /api/tools/beam, the public, session-less
// "download Beam" link. Its only job is to pick the platform off the
// User-Agent and hand off to the real download.
//
// It used to assemble a redirect to the beam-relay's /download/{platform}
// itself, and every step of that was wrong by the time it was found: the
// setting it preferred, beam.download_url, has no writer anywhere in the tree
// (the Beam settings tab writes beam.download_link, which this route ignored);
// the relay's binaries.go was deleted when the app was decoupled from it, so
// /download no longer exists there; the 302 handed the browser the relay's
// host, which GetBeamDownload deliberately stopped doing because that is
// usually an overlay address; the caller's platform reached the Location
// header unvalidated; and none of it touched the signed release manifest,
// which made this the one route that served an unverified executable. Its 503
// even told the operator to set the key that nothing can set.
//
// Redirecting into GetBeamDownload leaves one implementation owning manifest
// verification, upstream TLS and platform validation, and the binary is
// streamed through Core instead of fetched from a host the browser gets to
// see. A platform we cannot serve now yields that handler's 400/503 rather
// than a redirect to a relay endpoint that no longer answers.
func beamToolsRedirect(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")
	if platform == "" {
		// Without this, GetBeamDownload would answer the bare link with its
		// platform index (JSON), not a download.
		platform = detectBeamPlatform(r.UserAgent())
	}
	http.Redirect(w, r, "/api/beam/download?platform="+url.QueryEscape(platform), http.StatusFound)
}

// aclHandshakeStore adapts the Core store to redisacl.HandshakeStore. Primitive
// types only, keeping the redisacl package free of store/models imports.
type aclHandshakeStore struct {
	store *store.PostgresStore
	flags *services.FeatureFlags
}

func (a *aclHandshakeStore) GetNodeSecretEnc(id int) (string, error) {
	return a.store.GetNodeSecretEnc(id)
}
func (a *aclHandshakeStore) SetNodeSecretEnc(id int, enc string) error {
	return a.store.SetNodeSecretEnc(id, enc)
}

func (a *aclHandshakeStore) SetNodeSecretEncIfUnchanged(id int, prev, next string) (bool, error) {
	return a.store.SetNodeSecretEncIfUnchanged(id, prev, next)
}

func (a *aclHandshakeStore) ServerUUIDsByNode(nodeID int) ([]string, error) {
	servers, err := a.store.ListServersByNode(nodeID)
	if err != nil {
		return nil, err
	}
	uuids := make([]string, 0, len(servers))
	for _, s := range servers {
		uuids = append(uuids, s.UUID)
	}
	return uuids, nil
}

func (a *aclHandshakeStore) ResolveEnrollToken(plaintext string) (string, bool, error) {
	return a.store.ResolveNodeEnrollToken(plaintext)
}

func (a *aclHandshakeStore) ConsumeEnrollToken(plaintext string) (string, bool, error) {
	ownerID, _, ok, err := a.store.ConsumeNodeEnrollToken(plaintext)
	return ownerID, ok, err
}

func (a *aclHandshakeStore) BindEnrollWarpKey(enrollToken string, nodeID int) (bool, error) {
	return a.store.BindWarpKeyFromEnrollToken(enrollToken, nodeID)
}

func (a *aclHandshakeStore) GetNodePublicKeys(id int) (string, string, error) {
	return a.store.GetNodePublicKeys(id)
}

func (a *aclHandshakeStore) SetNodePublicKeyIfUnchanged(id int, prevKey, prevRejected, key string) (bool, error) {
	return a.store.SetNodePublicKeyIfUnchanged(id, prevKey, prevRejected, key)
}

func (a *aclHandshakeStore) NodeIDByToken(token string) (int, bool, error) {
	n, err := a.store.GetNodeByToken(token)
	if err != nil {
		return 0, false, nil // unknown token: not found, not an error for this path
	}
	return n.ID, true, nil
}

// NodeLimitReached enforces the BYON node-adoption cap on the gRPC enroll
// path: only enforced when BYON is on; a 0 / missing cap means unlimited;
// fail-open on store errors.
func (a *aclHandshakeStore) NodeLimitReached(ownerID string) bool {
	if a.flags == nil || !a.flags.IsBYONEnabled(context.Background()) {
		return false
	}
	lim, err := services.EffectiveLimits(a.store, ownerID)
	if err != nil || lim.MaxNodes == nil {
		return false
	}
	cnt, err := a.store.CountNodesByOwner(ownerID)
	if err != nil {
		return false
	}
	// Live nodes only, deliberately: the identity being redeemed right now is
	// still pending, so counting pending ones here would have it count against
	// itself and the last slot could never be claimed. See services.NodeSlotsUsed.
	return services.AtOrOver(lim.MaxNodes, int64(cnt))
}

// CreatePlatformNode is CreateBYONNode without the owner binding: the row stays
// owner_id NULL, which is what makes it an operator node rather than a tenant's.
//
// Its only caller is EnrollPlatform, after the cluster proof was verified, so the
// row is marked as cluster-minted. That marker is what lets the stale sweep take
// it: this node re-pairs by itself. An admin-created row never carries it.
func (a *aclHandshakeStore) CreatePlatformNode(token, address, displayName string) (int, error) {
	n := &models.Node{Name: token, Token: token, Address: address, Status: "offline"}
	if err := a.store.CreateNode(n); err != nil {
		return 0, err
	}
	if err := a.store.SetNodeEnrolledVia(n.ID, store.NodeEnrolledViaClusterProof); err != nil {
		return 0, fmt.Errorf("mark node %d as cluster-proof enrolled: %w", n.ID, err)
	}
	if displayName != "" {
		if err := a.store.SetNodeDisplayName(n.ID, displayName); err != nil {
			return 0, err
		}
	}
	return n.ID, nil
}

func (a *aclHandshakeStore) CreateBYONNode(token, address, ownerID, displayName string) (int, error) {
	n := &models.Node{Name: token, Token: token, Address: address, Status: "offline"}
	if err := a.store.CreateNode(n); err != nil {
		return 0, err
	}
	if err := a.store.SetNodeOwner(n.ID, &ownerID); err != nil {
		return 0, err
	}
	if displayName != "" {
		if err := a.store.SetNodeDisplayName(n.ID, displayName); err != nil {
			return 0, err
		}
	}
	return n.ID, nil
}

func main() {
	log.Println("Starting Dylaris Core...")

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("FATAL: Config Error: %v", err)
	}

	db, err := database.InitDB(cfg)
	if err != nil {
		log.Fatalf("FATAL: Database Error: %v", err)
	}
	defer db.Close()

	pgStore := store.NewPostgresStore(db)
	// Encrypt the credential settings (S3 secret keys) at rest, keyed from
	// CLUSTER_SECRET. Installed before any handler reads or writes a setting, so
	// the storage secrets are never persisted in the clear. A legacy plaintext
	// secret still reads through and is re-encrypted on its next save.
	pgStore.SetSettingsEncryptionKey(cfg.ClusterSecret)
	// Encrypt storage_connections secrets at rest with a distinct
	// CLUSTER_SECRET-derived key. Installed before any handler resolves a
	// connection, so a connection secret is never stored or read in the clear.
	pgStore.SetStorageConnEncryptionKey(cfg.ClusterSecret)
	// Encrypt backup_storages s3 secrets at rest with a distinct
	// CLUSTER_SECRET-derived key. Installed before any backup handler or the
	// scheduler resolves a storage, so the s3 secret is never stored in the
	// clear and a legacy plaintext one is re-encrypted on its next save.
	pgStore.SetBackupStorageEncryptionKey(cfg.ClusterSecret)

	// Which reverse-proxy networks may set X-Forwarded-For. Installed before any
	// handler serves, so the rate limiters and the audit log key on a client IP
	// that a direct client cannot forge. Process-global, read-only after this.
	handlers.SetTrustedProxies(cfg.TrustedProxyCIDRs)

	// gRPC Registry for Node connections
	grpcRegistry := nodegrpc.NewRegistry()

	appState := &handlers.AppState{
		Store:               pgStore,
		GRPCRegistry:        grpcRegistry,
		FrontendURL:         cfg.FrontendURL,
		PanelAPIURL:         cfg.PanelAPIURL,
		ExternalTicketDBURL: cfg.ExternalTicketDBURL,
		FeatureFlags:        services.NewFeatureFlags(pgStore),
		Authz:               authz.NewResolver(pgStore),
		DBType:              cfg.DBType,
		StoreEnabled:        cfg.StoreEnabled,
		StoreURL:            cfg.StoreURL,
		StoreSharedKey:      cfg.StoreSharedKey,
		TabProxyHostSuffix:  cfg.TabProxyHostSuffix,
		UpdatesURLPlatform:  cfg.UpdatesURLPlatform,
		UpdatesURLHosted:    cfg.UpdatesURLHosted,
		ReleaseVersion:      coreReleaseVersion().String(),
		CoreID:              cfg.CoreID,
	}

	// Demo showcase read access flows through the resolver so the RequireCap
	// chokepoint covers console/stats/overview reads on demo servers.
	appState.Authz.SetDemoRead(appState.IsDemoServerID)

	// Precompute the cluster-wide gRPC-TLS fingerprint once so handlers can hand it
	// to BYON operators without re-deriving. Non-secret; safe to expose.
	if fp, ferr := beamauth.ClusterGRPCCertFingerprint(cfg.ClusterSecret); ferr == nil {
		appState.GRPCTLSFingerprint = fp
	}
	appState.GRPCTLSEnabled = cfg.GRPCTLSEnabled
	if !cfg.GRPCTLSEnabled {
		log.Println("WARNING: GRPC_TLS_ENABLED is false; node<->Core gRPC (control channel, carries per-node secrets) is UNENCRYPTED. Rely on an encrypted overlay (WireGuard/VPN) between node and Core, or set GRPC_TLS_ENABLED=true.")
	}

	redisClient, err := database.InitRedis(cfg)
	if err != nil {
		log.Fatalf("FATAL: Redis Error: %v", err)
	}

	appState.Redis = redisClient
	appState.Queue = services.NewQueueService(redisClient)
	// The background services report their failures here as well as to the log,
	// so the panel's Errors screen can show them. Core is the component running
	// the periodic work and was the only one not writing to these streams; a
	// container's stdout does not survive its redeploy. See services/errsink.go.
	services.SetErrorSink(errlog.New(redisClient, "core", cfg.CoreID))
	// Metadata caches default into this same Redis. An operator who pointed them
	// at a dedicated endpoint gets it applied here; a failure is logged and the
	// caches stay on the default rather than being switched off, because a cache
	// that is silently absent only shows up as a slower panel.
	appState.Cache = services.NewCache(redisClient)
	if err := appState.ApplyCacheSettings(context.Background()); err != nil {
		log.Printf("mod metadata cache: the configured endpoint is not answering yet, so nothing is cached until it does (it is NOT falling back to the panel Redis): %v", err)
	}

	// In-panel cross-database migration. The source is THIS Core's live DB,
	// re-opened read-only as the copy source; the target is supplied per-request.
	appState.DBMigration = services.NewDBMigrationService(redisClient, pgStore, services.DBConnParams{
		Host:     cfg.DBHost,
		Port:     cfg.DBPort,
		User:     cfg.DBUser,
		Password: cfg.DBPassword,
		DBName:   cfg.DBName,
		SSLMode:  cfg.DBSSLMode,
		DBType:   cfg.DBType,
	})

	// In-panel blob storage migration (library, ticket-attachments,
	// ticket-backups, modpacks, server-backups rows). The resolver owns the
	// Core file storage config, the modpack settings and the backup_storages
	// rows, so it is wired from appState after appState.Store etc. are set.
	appState.StorageMigration = services.NewStorageMigrationService(
		redisClient, pgStore, handlers.NewStorageDataSetResolver(appState),
	)

	// Per-server CPU pinning: reads node topology from Redis + computes auto cpusets.
	appState.CPUPinning = services.NewCPUPinningService(redisClient, pgStore)

	// One cancellable context for every long-lived background loop below, so
	// shutdown can tell them to stop instead of relying on process exit. It is
	// cancelled after both HTTP listeners have drained, so a request that is
	// still in flight never loses a service it depends on mid-response.
	//
	// Deliberately NOT used for one-shot boot calls (a single Redis Set, the
	// host-path warning): those complete during boot, where this context is
	// never cancelled, so passing it would only suggest a lifecycle they do
	// not have.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// The two core-storage connection mechanisms. Exactly one of them is live
	// at a time, decided by which backend the config names.
	//
	// Both are constructed BEFORE SyncStorageGate, which points the watchdog at
	// the configured path and drops any stale s3 state. It touches both, so
	// creating either one after it would leave that half silently unsynced.
	appState.StorageGate = storage.NewGate()
	appState.StorageS3 = storage.NewS3Resilience()
	appState.SyncStorageGate()

	// The s3 recovery probe. It idles unless the backend is actually
	// reconnecting, and it is what lets a Core whose only storage traffic is
	// uploads notice that the object store came back instead of waiting out
	// the full retry budget.
	appState.StorageS3.StartProbe(bgCtx, appState.ProbeS3Connection)

	// System-events publisher. Mutating handlers (regions,
	// modules, features, maintenance, servers CRUD) drop events into a
	// single Redis Pub/Sub channel; panels subscribe via SSE so they refresh
	// without polling. Construction is cheap — wired before any handler so
	// every code path can call h.state.Events.Publish unconditionally.
	appState.Events = services.NewSystemEventsPublisher(redisClient)

	// Storage connection state onto the system-events channel. Wired AFTER the
	// publisher above, because it captures it. Start before Attach so the
	// forwarder is already draining when the first transition can fire.
	appState.StorageStatus = services.NewStorageStatus(appState.Events, appState.StorageGate, appState.StorageS3)
	appState.StorageStatus.Start(bgCtx)
	appState.StorageStatus.Attach()

	// Leader election: a single Redis lease named for the
	// "core-leader" role, identified by this instance's CoreID. Every
	// scheduled background loop consults the leader's IsLeader() to
	// decide whether to perform its work or idle. Single-instance Core
	// always wins the election so behavior is unchanged for dev. Multi-
	// instance Core safely converges on exactly one active leader.
	coreLeader := leader.New(redisClient, "dylaris:core:leader", cfg.CoreID)
	coreLeader.Start(bgCtx)

	discovery := services.NewDiscoveryService(pgStore, redisClient, cfg.ClusterSecret)
	discovery.SetLeader(coreLeader)
	discovery.Start()

	nodeCleanup := services.NewNodeCleanupService(pgStore, 24*time.Hour)
	nodeCleanup.SetLeader(coreLeader)
	nodeCleanup.Start()

	statusWatcher := services.NewStatusWatcherService(pgStore, redisClient)
	statusWatcher.SetLeader(coreLeader)
	statusWatcher.Start()

	statsConsumer := services.NewStatsConsumerService(pgStore, redisClient, cfg.CoreID)
	statsConsumer.Start()

	// Gateway bandwidth consumer — ingests the edge/warp/beam telemetry streams,
	// aggregates per swarm host, mirrors to Redis for the panel, and persists
	// downsampled rows into gateway_bandwidth_stats. Leader-gated persistence.
	gwBandwidth := services.NewGatewayBandwidthConsumerService(pgStore, redisClient, cfg.CoreID)
	gwBandwidth.SetLeader(coreLeader)
	gwBandwidth.Start(bgCtx)

	sftpSync := services.NewSFTPSyncService(pgStore, redisClient, appState.Authz)
	sftpSync.Start()

	// Traffic aggregator — leader-gated + BYON-gated. Turns the per-server byte
	// counters edges + relays publish to Redis into per-tenant monthly rows in
	// traffic_usage. No-op in solo/hoster mode (feature_byon_enabled off).
	trafficAggregator := services.NewTrafficAggregator(pgStore, redisClient, appState.FeatureFlags)
	trafficAggregator.SetLeader(coreLeader)
	trafficAggregator.Start(bgCtx)

	// Long-term metrics — leader-gated and OFF by default
	// (feature_metrics_enabled). Records what the platform handled over months,
	// into minute buckets in a statistics database of its own. Nothing leaves
	// the installation: telemetry that phoned home was removed in full and the
	// README says so.
	//
	// With no statistics database configured, nothing is recorded at all. There
	// used to be a fallback into this database at hour resolution; it is gone,
	// because two resolutions with no conversion between them meant an operator
	// could end up with a year of history at the one they did not want.
	//
	// An unreachable statistics database is logged and skipped, never fatal. It
	// is a statistics store; it must not be a reason Core does not come up.
	//
	// The TARGET is a panel setting and nothing else - boot reads the same
	// stored row the settings screen writes, so the two cannot disagree about
	// which database is being written.
	metricsManager := metrics.NewManager(bgCtx)
	appState.Metrics = metricsManager
	defer metricsManager.Close()

	if mErr := metricsManager.Apply(services.LoadMetricsDBTarget(pgStore).DSN()); mErr != nil {
		// Not fatal and not permanent. During a stack deploy this Core can
		// start before its statistics database is resolvable, and the manager
		// keeps trying - see the note on Manager.want.
		log.Printf("metrics: not recording yet, will keep trying (%v)", mErr)
	}
	// Started regardless of whether that target opened. The collector records
	// through the manager rather than holding a recorder, so an admin who fixes
	// the database in the panel gets recording without a restart - which is the
	// whole reason the manager exists.
	metricsCollector := services.NewMetricsCollector(pgStore, redisClient, db, metricsManager, appState.FeatureFlags)
	metricsCollector.SetLeader(coreLeader)
	// The gateway telemetry this Core already consumes. Reading it through
	// the existing consumer keeps one set of Redis consumer groups per
	// stream instead of two per Core.
	metricsCollector.SetGatewayStats(gwBandwidth)
	metricsCollector.Start(bgCtx)

	// Billing lifecycle — leader-gated. Progresses past_due tenants whose grace
	// window has elapsed into suspended (hard cutoff deferred to SuspendGrace
	// later, keeps data). Payment-provider-agnostic; handlers/webhooks call
	// EnterPastDue/Reactivate/Suspend.
	appState.Billing = services.NewBillingLifecycleService(pgStore, appState.Queue, grpcRegistry, cfg.FrontendURL, cfg.SuspendGrace, cfg.StoreEnabled)
	appState.Billing.SetLeader(coreLeader)
	// Start() is deferred until after SetLinkACL below, so the ticker can never run
	// a suspend before the link teardown dependencies are wired.

	// DNS records are written by the gateway HUB, not here. Everything this
	// subsystem ever managed - the edge wildcards and the beam relay names - is a
	// gateway name, and a Hub runs in every gateway deployment, so keeping the
	// writer here left a standalone gateway with nobody to write them. Two writers
	// would have been worse than none: the reconciler deletes records it does not
	// plan, and its "an edge name wins over a relay name" guard only holds inside
	// one plan.
	//
	// A DNS_* variable still set here is reported rather than ignored - see
	// warnDNSMovedToHub - because "it stopped working" and "it moved, here is
	// where" are a long afternoon apart.
	warnDNSMovedToHub(&cfg)

	// Auto-delete service — daily ticker scans inactive users,
	// emails warnings, executes deletions per the auth.* settings. No-op
	// unless the operator turns it on. Leader-gated so only one Core runs
	// it under multi-instance.
	//
	// STARTED further down, after the ACL provisioner exists: removing an account
	// has to remove what the account ran, and this service cannot do that until
	// it has been given the link plane. Start() runs a tick immediately, so
	// starting it here would mean the first sweep of every boot is the one that
	// tears nothing down.
	autoDelete := services.NewAutoDeleteService(pgStore, cfg.FrontendURL)
	autoDelete.SetLeader(coreLeader)

	// Ticket auto-close — daily ticker, leader-gated. No-op until
	// the operator turns it on via Settings → Ticket Settings.
	ticketAutoClose := services.NewTicketAutoCloseService(pgStore)
	ticketAutoClose.SetLeader(coreLeader)
	ticketAutoClose.Start(bgCtx)

	// Server-audit retention sweep — daily, leader-gated. No-op
	// when audit.server_retention_days is 0 (keep forever) or unset.
	serverAuditRetention := services.NewServerAuditRetentionService(pgStore)
	serverAuditRetention.SetLeader(coreLeader)
	serverAuditRetention.Start(bgCtx)

	// Ticket-audit retention sweep — the same thing for tickets.audit_retention_days,
	// which was settable and stored but had no consumer at all.
	ticketAuditRetention := services.NewTicketAuditRetentionService(pgStore)
	ticketAuditRetention.SetLeader(coreLeader)
	ticketAuditRetention.Start(bgCtx)

	// Warp leaders register themselves — leader-gated, every minute. A leader
	// runs as a global Swarm service keyed on the node hostname, so every host
	// added to the edge tier starts one with an id nobody typed, and the public
	// endpoint it needs is a value only that host knows for certain. Both ride
	// in the liveness key the leader already refreshes.
	warpSelfReg := services.NewWarpSelfRegistrar(pgStore, redisClient)
	warpSelfReg.SetLeader(coreLeader)
	warpSelfReg.Start(bgCtx)

	// Route-only routes are published straight into Redis, which used to be the
	// only place they existed. This writes the stored ones back, so a Redis that
	// lost its keys costs a minute rather than every route-only route the
	// platform had.
	routeRepublisher := services.NewRouteRepublisher(pgStore, redisClient)
	routeRepublisher.SetLeader(coreLeader)
	routeRepublisher.Start(bgCtx)

	// Tells each node which of its servers may be reached by which other server.
	// The node refuses everything else at the container itself; see
	// services/netpolicy_publisher.go and platform/node/netpolicy.go.
	//
	// A node enforces NOTHING until this has published once, deliberately: a
	// Core that knew nothing about the policy must not be able to cut a linked
	// proxy off from its backends.
	netPolicy := services.NewNetPolicyPublisher(pgStore, redisClient)
	netPolicy.SetLeader(coreLeader)
	netPolicy.Start(bgCtx)
	appState.NetPolicy = netPolicy

	// Modpack auto-update checker — hourly, leader-gated. Pauses when the
	// modpacks feature is off; per-row staleness governed by the admin cadence
	// setting (modpack_update_check_interval_hours, default 24h).
	modpackUpdateChecker := services.NewModpackUpdateChecker(pgStore, appState.FeatureFlags)
	modpackUpdateChecker.SetLeader(coreLeader)
	modpackUpdateChecker.Start(bgCtx)

	// Fallback cleanup if TimescaleDB retention policy is not active.
	// Leader-gated so under multi-Core only one instance
	// fires the hourly DELETE — the followers idle on the tick.
	go func() {
		for range time.NewTicker(1 * time.Hour).C {
			if !coreLeader.IsLeader() {
				continue
			}
			db.Exec("DELETE FROM server_stats WHERE time < NOW() - INTERVAL '24 hours'")
			db.Exec("DELETE FROM gateway_bandwidth_stats WHERE time < NOW() - INTERVAL '24 hours'")
		}
	}()

	// Gateway always active — uses same Redis as Core
	redisGateway := services.NewRedisGateway(redisClient, pgStore, cfg.ClusterSecret)
	appState.Gateway = redisGateway

	// Which link serves each node is the Hub's answer (hub:node-link:<node>), not
	// a token Core derives: a self-enrolled Link carries a generated one, and the
	// Hub drops a route whose token names no link. Leader-gated, every minute,
	// beside the warp self-registration it is modelled on.
	nodeLinks := services.NewNodeLinkLearner(pgStore, redisGateway)
	nodeLinks.SetLeader(coreLeader)
	nodeLinks.Start(bgCtx)

	// Routing migration service for batch redeployment when mode changes
	appState.RoutingMigration = services.NewRoutingMigrationService(pgStore, appState.Queue, redisClient)

	// Migration orchestrator — leader-gated consumer of the node-to-node
	// migration (auto-move) queue. Manual + (Wave 4) rebalance moves enqueue
	// requests; only the elected Core runs the step machine. Exposed on
	// AppState so the manual-move endpoint can EnqueueMigration.
	migrationOrchestrator := services.NewMigrationOrchestrator(pgStore, redisClient, appState.Queue, appState.Gateway, cfg.ClusterSecret)
	migrationOrchestrator.SetLeader(coreLeader)
	migrationOrchestrator.Start(bgCtx)
	appState.Migration = migrationOrchestrator
	migrationOrchestrator.SetCPUPinning(appState.CPUPinning)

	// Rebalance worker — leader-gated ticker that migrates eligible (auto_move,
	// 0-player) servers off overloaded nodes by enqueuing onto the orchestrator.
	// No-op unless auto-move is enabled AND gateway routing is active.
	rebalanceWorker := services.NewRebalanceWorker(pgStore, redisClient, migrationOrchestrator, appState.FeatureFlags)
	rebalanceWorker.SetLeader(coreLeader)
	rebalanceWorker.Start(bgCtx)

	// Warp rebalancer — leader-gated ticker that relieves saturated warp leaders
	// by pinning individual peers to a freer same-region sibling. No-op unless
	// warp_rebalance_mode is dry-run/armed AND gateway routing is active.
	warpRebalancer := services.NewWarpRebalancer(pgStore, redisClient, appState.FeatureFlags)
	warpRebalancer.SetLeader(coreLeader)
	warpRebalancer.Start(bgCtx)

	// Publish the node-facing settings to Redis now, and keep re-publishing
	// them: Redis has no persistence, and a node restarting into a wiped Redis
	// silently falls back to its compiled defaults (see NodeModePublisher).
	services.NewNodeModePublisher(pgStore, redisClient).Start(bgCtx)

	// Initialize handlers
	authHandler := handlers.NewAuthHandler(appState, cfg.JWTSecret)

	// Warp: external/home node WireGuard bridge (multi-hub registry).
	// Seed a default region from any pre-multi-hub settings so an existing
	// single-hub deployment keeps working unchanged. Region id "leader-01" keeps
	// the WG key derived from CLUSTER_SECRET+region byte-identical to the old
	// single-leader key, so already-enrolled peers stay valid.
	{
		seedSubnet, _ := pgStore.GetSetting("warp:client_subnet")
		if seedSubnet == "" {
			seedSubnet = "10.0.99.0/24"
		}
		seedEndpoint, _ := pgStore.GetSetting("warp:leader_endpoint")
		if err := pgStore.SeedWarpRegionIfEmpty("leader-01", seedSubnet, "leader-01", seedEndpoint); err != nil {
			log.Printf("warp: seed default region: %v", err)
		}
	}

	// Backfill the region-subnet Redis mirror for every existing region. The seed
	// block above only fires on an empty regions table, so a Core restart against
	// an already-populated table needs this to repopulate the mirror a warp leader
	// reads at boot.
	if regions, err := pgStore.ListWarpRegions(); err == nil {
		for _, rg := range regions {
			if err := services.PublishRegionSubnet(context.Background(), redisClient, rg.Region, rg.Subnet); err != nil {
				log.Printf("warp: region subnet mirror backfill failed for %s: %v", rg.Region, err)
			}
		}
	} else {
		log.Printf("warp: region subnet backfill: list regions failed: %v", err)
	}

	// The panel bundle, served out of this binary. A failure here is fatal: a
	// Core with no panel is an API nobody can reach a screen through, and a
	// process that boots anyway would look healthy while every browser gets a
	// 404.
	panel, err := panelfs.New(cfg.PanelAPIURL, cfg.TabProxyHostSuffix)
	if err != nil {
		log.Fatalf("panel bundle: %v", err)
	}
	if !panel.Built {
		log.Printf("panel: WARNING - this binary carries the placeholder bundle, not a real build. " +
			"Every panel URL will answer with a page telling the operator so. The API is unaffected.")
	} else {
		log.Printf("panel: serving %d routes from the embedded bundle", panel.Routes())
	}

	root, extras := buildAPIRouter(appState, authHandler, routeCfg{
		Panel:              panel,
		JWTSecret:          cfg.JWTSecret,
		Region:             cfg.Region,
		CoreID:             cfg.CoreID,
		TabProxyHostSuffix: cfg.TabProxyHostSuffix,
		ClusterSecret:      cfg.ClusterSecret,
		GatewayHubURL:      cfg.GatewayHubURL,
		ModrinthUA:         "Dylaris/0.10 (+https://github.com/Bartis-Dev/dylaris-platform)",
	})
	// boot-time warp resync watcher + firewall-allowlist publish use the
	// handlers/service buildAPIRouter just constructed.
	extras.warpService.StartResyncWatcher(coreLeader.IsLeader)

	// Custom-domain ownership proof. Tenants get a grant to point their own
	// domain at us; this is what enforces the deadline and removes the route when
	// it passes unproven.
	//
	// Leader-gated: every Core replica sees the same pending claims, and a
	// non-leader running this too would race to delete the same routes and count
	// the same failure more than once - which, at two failures, is the difference
	// between a retry and a permanent block.
	{
		customDomainVerifier := services.NewCustomDomainVerifier(
			pgStore,
			services.NewNetResolver(),
			services.NewCustomDomainRouteRemover(redisClient, appState.Gateway),
			func() ([]string, []string) {
				// gateway_cname_target is a LABEL ("route"), never a usable name.
				// Passing it through raw compared a resolved CNAME against
				// "route", which no DNS answer can equal, so the CNAME half of
				// the proof could not pass for anyone - see services.CNAMETargets.
				hosters, _, cname := extras.settingsHandler.LoadGatewayDomainConfig()
				bases := make([]string, 0, len(hosters))
				for _, h := range hosters {
					bases = append(bases, h.Domain)
				}
				return services.CNAMETargets(cname, bases),
					services.OnlineEdgeIPs(context.Background(), redisClient)
			},
		)
		// Gated per pass, not once at boot: coreLeader.Start above only launches
		// the election goroutine, so IsLeader() is still false here on a cold
		// start, and leadership moves at runtime anyway.
		customDomainVerifier.SetLeader(coreLeader)
		customDomainVerifier.Start(context.Background())
	}
	// Publish the spoke firewall allowlist to the central Redis key the warp
	// leaders read and poll, so a freshly (re)started leader gets the admin value
	// rather than only its compiled-in fail-closed default. Always write (even
	// the default) so a stale value from a previous install cannot linger.
	if err := redisClient.Set(context.Background(), handlers.WarpFirewallRedisKey, extras.settingsHandler.LoadWarpSpokeAllowedPorts(), 0).Err(); err != nil {
		log.Printf("WARNING: failed to publish warp-firewall allowlist to Redis at boot; leaders may still be running a stale/compiled-in default until the next successful save: %v", err)
	}

	// gRPC Server for Node connections (NodeService)
	grpcLookup := &nodegrpc.StoreAdapter{
		GetByToken: func(token string) (int, bool, error) {
			node, err := pgStore.GetNodeByToken(token)
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nodegrpc.ErrNodeNotFound
			}
			if err != nil {
				return 0, false, err
			}
			return node.ID, node.Kind() == models.NodeKindBYON, nil
		},
	}
	// Per-node Redis-ACL handshake. Runs on every node connect (Redis ACL is
	// mandatory); provisions the node's scoped ACL users and mints/returns its
	// per-node secret.
	aclProvisioner := redisacl.NewProvisioner(redisClient)
	// Hand the warp handler Core's ACL provisioner + cluster secret so the
	// route-only link-boot endpoint can derive and provision per-link creds.
	appState.ACLProvisioner = aclProvisioner
	appState.ClusterSecret = cfg.ClusterSecret

	// How Core reaches its OWN database with pg_dump / pg_restore, for platform
	// backups. The major version is asked once here rather than per run: it
	// decides which installed client is used, and the constraint points both
	// ways - pg_dump refuses a server newer than itself, pg_restore emits SQL an
	// older one does not understand.
	appState.PlatformDB = services.PGConn{
		Host: cfg.DBHost, Port: cfg.DBPort, User: cfg.DBUser,
		Password: cfg.DBPassword, Name: cfg.DBName, SSLMode: cfg.DBSSLMode,
	}
	if major, merr := services.PGServerMajor(db); merr == nil {
		appState.PlatformDBMajor = major
	} else {
		// Not fatal: everything except a platform backup works without it, and
		// the client choice then falls back to whatever is on PATH.
		log.Printf("could not read the database major version, platform backups will guess the client: %v", merr)
	}
	// The same volume the library and ticket attachments already live on, so a
	// bundle is spooled where there is room for it rather than on the
	// container's own writable layer.
	appState.PlatformBackupWorkDir = filepath.Join("dylaris_data", "platform-backups")
	appState.GatewayHubURL = cfg.GatewayHubURL
	appState.AdminSecret = cfg.AdminSecret
	appState.SetupEnabled = cfg.SetupEnabled
	appState.SuspendGrace = cfg.SuspendGrace

	// ACL reconciler - leader-gated. Periodically (and on a Redis reconnect)
	// re-provisions every paired node's + route-only link's scoped Redis ACL
	// users from the DB-stored per-node secret, so a Valkey restart that lost the
	// aclfile self-heals without a service restart. Same users, same passwords;
	// running services re-auth transparently on their next command.
	aclReconciler := services.NewACLReconciler(pgStore, aclProvisioner, redisClient, cfg.ClusterSecret, cfg.SuspendGrace)
	aclReconciler.SetLeader(coreLeader)
	aclReconciler.Start(bgCtx)
	// The billing lifecycle drops/restores route-only link tunnels on
	// suspend/reactivate; give it the same provisioner, gateway and cluster secret.
	appState.Billing.SetLinkACL(appState.Gateway, redisClient, aclProvisioner, cfg.ClusterSecret)
	// ...and the warp service, so the hard cutoff can drop the tenant's overlay
	// tunnel itself. Taking away what the tunnel carries is not the same as
	// taking away the tunnel, and only this drops the WireGuard peer.
	appState.Billing.SetWarpPeers(extras.warpService)
	appState.Billing.Start(bgCtx)
	// Same three values, same reason: the auto-delete sweep removes accounts, and
	// an account's link kit holds a Redis credential and a tunnel key that
	// nothing else will ever clean up once its row is gone.
	autoDelete.SetLinkACL(appState.Gateway, redisClient, aclProvisioner)
	autoDelete.Start(bgCtx)
	aclHandshake := redisacl.NewHandshake(
		&aclHandshakeStore{store: pgStore, flags: appState.FeatureFlags},
		aclProvisioner,
		cfg.ClusterSecret,
	)
	// P0b-5 admission gate. The JOIN half is consulted on the ACL-on gRPC enroll
	// path for unknown nodes; the NETWORK half runs on the warp enrol instead,
	// which is the only place a BYON customer's real IP is visible (over gRPC the
	// peer is the warp leader, identical for every customer).
	admissionGate := services.NewAdmissionGate(pgStore)
	appState.Admission = admissionGate
	// Pre-placement ACL hook: re-apply a node's Redis ACL right before sending a
	// server-placement command, closing the window where a freshly created
	// server's keys are NOPERM for the node until its next reconnect. nil-safe.
	appState.Queue.SetACL(aclHandshake)
	// Serves in its own goroutine; the handle comes back so shutdown can drain
	// node streams instead of severing them. A bind failure surfaces here,
	// synchronously, rather than from inside a goroutine after boot continued.
	// The gRPC package imports neither models nor store, so the join-attempt
	// recorder is adapted here rather than satisfied directly - the same shape
	// StoreAdapter already uses for node lookups.
	joinAttempts := &nodegrpc.JoinAttemptFuncs{
		Record: func(a nodegrpc.JoinAttempt) error {
			return pgStore.RecordNodeJoinAttempt(models.NodeJoinAttempt{
				NodeToken:          a.NodeToken,
				PeerIP:             a.PeerIP,
				ReportedPublicIP:   a.PublicIP,
				ReportedPrivateIPs: a.PrivateIPs,
				Hostname:           a.Hostname,
				CPUCores:           a.CPUCores,
				CPUModel:           a.CPUModel,
				MemoryBytes:        a.MemoryBytes,
				ReleaseVersion:     a.ReleaseVersion,
				Reason:             a.Reason,
			})
		},
		Consume:       pgStore.ConsumeNodeJoinApproval,
		Forget:        pgStore.DeleteNodeJoinAttempt,
		Authenticated: pgStore.SetNodeLastAuthPeerIP,
	}
	// Nodes are told the REDIS_ADDR Core was configured with, read raw rather
	// than as cfg.RedisAddr: unset, that falls back to localhost, which names
	// Core's own loopback and would send every node to itself. Unset here means
	// Core names none, and a node keeps the address it has.
	grpcServer, err := nodegrpc.StartGRPCServer(cfg.GRPCPort, grpcRegistry, grpcLookup, cfg.CoreID, aclHandshake, appState.Gateway, admissionGate, joinAttempts, cfg.GRPCTLSEnabled, cfg.ClusterSecret, strings.TrimSpace(os.Getenv("REDIS_ADDR")))
	if err != nil {
		log.Fatalf("gRPC server error: %v", err)
	}

	// Core Heartbeat in Redis (so Nodes can discover this Core)
	coreHeartbeat := services.NewCoreHeartbeatService(redisClient, cfg.CoreID, cfg.Region, coreReleaseVersion().String(), cfg.GRPCPort)
	coreHeartbeat.Start()

	// Shared-storage reachability. This Core PROVES it can write to and read
	// from the same core file storage as its peers - at boot, then every
	// 120s - and gates its own storage routes when it cannot. Deliberately
	// not leader-gated: every Core must verify itself, and only itself.
	appState.StorageReach = storagereach.NewService(storagereach.ServiceDeps{
		Redis:       redisClient,
		CoreID:      cfg.CoreID,
		NewProvider: appState.NewReachProvider,
		ConfigFor: func() (storagereach.Config, bool) {
			cfg, err := appState.EffectiveCoreStorageConfig()
			if err != nil || !appState.CoreStorageConfigured() {
				return storagereach.Config{}, false
			}
			return handlers.CoreStorageToReachConfig(cfg), true
		},
		OnlineCores: func(ctx context.Context) ([]string, error) {
			return services.OnlineCoreIDs(ctx, redisClient)
		},
		Publish: func(eventType string, payload map[string]interface{}) {
			appState.Events.Publish(context.Background(), eventType, payload)
		},
	})
	appState.StorageReach.Start(bgCtx)
	// Announce it like every other background service. Without this line the one
	// service whose job is to make a SILENT storage failure visible is itself
	// invisible in the boot log, so an operator cannot tell a Core that verified
	// and passed from one where the check never ran.
	log.Printf("Storage Reachability Verifier started (id=%s)", cfg.CoreID)

	// Backup scheduler — ticks once a minute, dispatches due jobs to nodes.
	// Wire the gRPC mesh in so retention deletes can reach node-local stores.
	// Leader-gated: tick + Pub/Sub result processing run only on
	// the elected Core to avoid double-dispatch and double-result-write.
	backupScheduler := services.NewBackupScheduler(pgStore, redisClient, appState.Queue)
	backupScheduler.SetRegistry(grpcRegistry)
	// Same builder handlers/backup.go supplies, so a job stored on the shared
	// Core file storage behaves identically whether it is reached from a
	// request or from the scheduler. Without it the scheduler is refused by
	// backupstorage.Open for exactly those jobs.
	backupScheduler.SetCoreStorage(appState.CoreStorageBackupBuilder())
	backupScheduler.SetConnection(appState.ConnectionBackupBuilder())
	backupScheduler.SetStoreEnabled(cfg.StoreEnabled)
	backupScheduler.SetLeader(coreLeader)
	backupScheduler.Start(bgCtx)

	// Mod installs report their outcome on the same per-node channel shape the
	// backup runs use. Leader-gated for the same reason: Pub/Sub reaches every
	// replica, and each one would apply the same report.
	services.NewModInstallResultService(pgStore, redisClient, coreLeader).Start(bgCtx)
	// What an installer FOUND, as opposed to what Core told it to do. Only a
	// backup import reports anything; see services.SetupResultService.
	services.NewSetupResultService(pgStore, redisClient, coreLeader).Start(bgCtx)

	// Platform backups on their schedule, and the retention that prunes them.
	// Without this the schedule field on the screen would be a control that
	// does nothing, which is worse than an absent one because somebody believes
	// it.
	platformBackups := handlers.NewPlatformBackupHandler(appState)
	services.NewPlatformBackupScheduler(pgStore, coreLeader,
		platformBackups.Runner, platformBackups.DeleteRunArchive).Start(bgCtx)

	// Scheduled-tasks executor — per-server cron jobs (restart, say).
	// Leader-gated, 30s tick. Publishes scheduled_tasks.changed via the SSE
	// channel after each dispatch so the panel updates last-run/next-run.
	scheduledTasksService := services.NewScheduledTaskService(pgStore, redisClient, appState.Queue, appState.Events)
	scheduledTasksService.SetLeader(coreLeader)
	scheduledTasksService.Start(bgCtx)

	// Recovery-token printer. Background loop that logs either the
	// Fresh-Install hint or the Lost-Admin token + URL every 30s as long as
	// the platform has no admin. Stops at the ctx cancel triggered by SIGTERM.
	services.StartSetupRecoveryLoop(bgCtx, pgStore, cfg.FrontendURL)

	// allowedOrigin gates CORS. Three classes are allowed, and NO vendor host is
	// ever trusted implicitly - an empty FRONTEND_URL never falls back to a
	// dylaris.com origin:
	//   1. The explicitly configured public Panel origin (FRONTEND_URL). The
	//      operator sets this per deployment; it is the ONLY way a public origin
	//      is trusted.
	//   2. Any localhost / private-LAN origin (localhost, 127/8, ::1, RFC1918,
	//      IPv6 ULA) on any port, so a self-hoster reaches the panel over
	//      localhost or a LAN IP without configuring anything.
	//   3. The Beam Desktop App, which runs the Panel inside a Wails webview
	//      whose origin is http://wails.localhost (Windows) or wails://wails.localhost
	//      (macOS/Linux).
	//   4. The share-wrapper origin, when TAB_PROXY_HOST_SUFFIX is set. The
	//      wrapper is the same panel bundle reached at the bare suffix, and it
	//      asks Core which content host a share token belongs to. It is NOT a
	//      tab-content host - those never reach this router at all, they are
	//      taken by the host mux before CORS.
	// This list is not an authorization decision, and must not become one. No
	// Access-Control-Allow-Credentials is emitted here, so a browser refuses to
	// hand any of these origins a response to a credentialed request - the
	// session cookie is host-only to the panel anyway, and an Authorization
	// header is not ambient authority. Adding credentials to this CORS config
	// would turn every origin above into one that can act as the signed-in user.
	shareWrapperOrigin := handlers.TabProxyWrapperOrigin(cfg.FrontendURL, cfg.TabProxyHostSuffix)
	allowedOrigin := func(origin string) bool {
		if origin != "" && origin == cfg.FrontendURL {
			return true
		}
		if origin != "" && shareWrapperOrigin != "" && origin == shareWrapperOrigin {
			return true
		}
		if config.IsLocalOrigin(origin) {
			return true
		}
		if strings.HasPrefix(origin, "wails://") {
			return true
		}
		if u, err := url.Parse(origin); err == nil && u.Hostname() == "wails.localhost" {
			return true
		}
		return false
	}

	corsObj := gorillaHandlers.CORS(
		gorillaHandlers.AllowedOriginValidator(allowedOrigin),
		gorillaHandlers.AllowedMethods([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}),
		gorillaHandlers.AllowedHeaders([]string{"Content-Type", "Authorization"}),
	)

	port := cfg.APIPort
	if port == "" {
		port = "25500"
	}

	log.Printf("Dylaris Core API running on port %s", port)
	// ReadHeaderTimeout defends against Slowloris (slow header drip) without
	// capping body read/write time — Core serves SSE streams and multi-GB
	// uploads/downloads, so ReadTimeout/WriteTimeout are deliberately left
	// unset. IdleTimeout reaps idle keep-alive connections.
	// Tab-content hosts are dispatched on the WHOLE request, ahead of CORS and
	// ahead of the router. Adding them as routes instead would leave the API and
	// every panel route answering on an origin that serves a tenant's container,
	// one fetch away from anything else this process exposes. No suffix
	// configured returns the handler untouched.
	srv := &http.Server{
		Addr: ":" + port,
		Handler: handlers.TabProxyHostMux(
			appState, extras.proxyHandler, corsObj(root)),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Core API crashed: %v", err)
		}
	}()

	// Graceful shutdown: mirrors node/main.go's signal handling. Rolling
	// deploys (docker-stack.yml start-first, 2 core replicas) SIGTERM the
	// outgoing task while the new one is already serving; without this the
	// old task hard-kills mid-request, dropping in-flight backup/migration
	// jobs and live SSE (/api/system/events) subscribers.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down Core gracefully...")

	// Teardown order is load-bearing and runs outside-in: stop taking work,
	// then stop doing work, then drop the connections both needed.
	//
	//  1. Drain the HTTP listeners. Requests in flight still have every
	//     background service and both Redis and Postgres under them.
	//  2. Drain node gRPC streams, which HTTP requests drive (file transfers,
	//     the tab-proxy bridge), so they cannot be cut from under a request.
	//  3. Cancel the background loops.
	//  4. Give up this Core's identity in Redis: delete the heartbeat, release
	//     the leader lease. Both write to Redis, so both must precede its close.
	//  5. Close Redis last.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown waits for active connections and gives up when the context
		// expires; it does not hang up on the stragglers. Close does, and
		// without it they are cut by process exit anyway - a beat later and
		// without their connection being closed.
		log.Printf("Core graceful shutdown error, closing remaining connections: %v", err)
		if cerr := srv.Close(); cerr != nil {
			log.Printf("Core listener close error: %v", cerr)
		}
	}
	// GracefulStop sends GOAWAY and then waits for every pending RPC. A node
	// stream is a long-lived RPC, so a node that has gone silent rather than
	// disconnected keeps this waiting until server keepalive tears its
	// transport down (Time 30s / Timeout 10s). Bound it and fall back to Stop,
	// which closes the transports outright; that also releases a GracefulStop
	// still blocked on them, which is why the goroutine is joined afterwards
	// instead of abandoned.
	grpcStopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(grpcStopped)
	}()
	select {
	case <-grpcStopped:
	case <-time.After(10 * time.Second):
		log.Println("gRPC: node streams did not drain in 10s, closing them")
		grpcServer.Stop()
		<-grpcStopped
	}

	// Background loops stop here rather than earlier: until this point a
	// request being drained above could still depend on one of them.
	bgCancel()

	// Stop being discoverable before Redis goes away. The heartbeat key carries
	// a 30s TTL and the leader lease a 30s TTL, so skipping either leaves this
	// instance counted as online, and its lease unavailable to a successor, for
	// that long after the process is gone.
	heartbeatCtx, heartbeatCancel := context.WithTimeout(context.Background(), 5*time.Second)
	coreHeartbeat.Stop(heartbeatCtx)
	heartbeatCancel()
	coreLeader.Stop()

	if err := redisClient.Close(); err != nil {
		log.Printf("Redis close error: %v", err)
	}

	log.Println("Core shutdown complete")
}
