package store

import (
	"context"
	"dylaris-core/models"
	"time"
)

// SFTPAccess is one CANDIDATE (user, server) pair returned by
// GetSFTPAccessByNode: someone the grant tables connect to a server on that
// node. It is not an authorization decision - the caller resolves it.
//
// IsOwner marks the rows that cannot fail that resolution, because the
// resolver's own short-circuit is keyed on exactly this. Every other row has to
// be resolved before its credentials are published: an invite carries a server
// role and capability overrides, and without checking them an invite with no
// capabilities at all opened a full read/write SFTP session.
type SFTPAccess struct {
	ServerUUID string
	ServerName string
	ServerID   int
	UserID     string
	Username   string
	IsOwner    bool
}

// Store defines all database operations of the Core
type Store interface {
	// --- Users ---
	GetUserByUsername(username string) (*models.User, error)
	GetUserByID(id string) (*models.User, error)
	CreateUser(user *models.User) error
	UsernameTaken(username, excludeUserID string) (bool, error)
	ListUsernameCaseCollisions() ([][]string, error)
	UpdateUser(user *models.User) error
	UpdateUserPassword(id string, hashedPassword string) error
	DeleteUser(id string) error
	ListUsers() ([]models.User, error)
	CountUsers() (int, error)

	// --- 2FA (TOTP + Backup Codes) ---
	SetUserTOTP(id string, secret string, backupCodesJSON string, enabled bool) error
	DisableUserTOTP(id string) error

	// --- Nodes ---
	GetNodeByID(id int) (*models.Node, error)
	GetNodeByToken(token string) (*models.Node, error)
	GetNodeByName(name string) (*models.Node, error)
	ListNodes() ([]models.Node, error)
	ListNodesByOwner(ownerID string) ([]models.Node, error)
	CreateNode(node *models.Node) error
	DeleteNode(id int) error
	SetNodeStatus(id int, status string) error
	SetNodeTags(id int, tags string) error
	SetNodeName(id int, name string) error
	SetNodeAddress(id int, address string) error
	SetNodeIPs(id int, publicIP string, privateIPs []string) error
	UpdateNodeCpusetCpus(id int, cpusetCpus string) error
	SetNodeOwner(id int, ownerID *string) error
	GetNodeSecretEnc(id int) (string, error)
	SetNodeSecretEnc(id int, enc string) error
	SetNodeDisplayName(id int, name string) error
	// --- BYON node enrollment ---
	CreateNodeEnrollToken(userID, plaintext, label string, expiresAt *time.Time) error
	ResolveNodeEnrollToken(plaintext string) (userID string, ok bool, err error)
	ConsumeNodeEnrollToken(plaintext string) (userID string, recoversNodeToken string, ok bool, err error)
	ListNodeEnrollTokens(userID string) ([]NodeEnrollToken, error)
	CountPendingNodeEnrollTokens(userID string) (int, error)
	DeleteNodeEnrollToken(id, userID string) error
	CreateRecoveryToken(userID, plaintext, nodeToken string, expiresAt *time.Time) error
	ResolveRecoveryToken(plaintext string) (recoversNodeToken string, ok bool, err error)
	// --- P0b-5 node admission ---
	ConsumeOneShotJoin() (won bool, err error)
	AddAdmissionCIDR(cidr, label string) error
	ListAdmissionCIDRs() ([]AdmissionCIDR, error)
	DeleteAdmissionCIDR(id string) error
	// --- BYON traffic metering ---
	TenantServerOwners() (map[string]string, error)
	TenantBackupBytes() (map[string]int64, error)
	AddTrafficUsage(userID string, period time.Time, edgeBytes, relayBytes int64) error
	// Per-region breakdown of the same edge bytes. A breakdown, never a second
	// source of truth - see AddTrafficUsageRegion.
	AddTrafficUsageRegion(userID string, period time.Time, region, kind, product string, bytes int64) error
	GetTrafficUsageRegions(userID string, period time.Time) ([]RegionUsage, error)
	SetTrafficBackupBytes(userID string, period time.Time, backupBytes int64) error
	GetTrafficUsage(userID string, period time.Time) (*TrafficUsage, error)
	ListTrafficUsage(period time.Time) ([]TrafficUsage, error)
	// --- BYON billing lifecycle ---
	GetUserBilling(userID string) (*UserBilling, error)
	SetUserBillingStatus(userID, status string, graceUntil, suspendedAt *time.Time) error
	SetUserBillingOverrides(userID, gracePeriod, r2Retention, nodeRetention string, r2QuotaGB *int64) error
	// SetUserManualEntitlement grants or revokes an admin entitlement
	// ("byon" | "route_only" | "both", empty = revoke) with an expiry.
	SetUserManualEntitlement(userID, kind string, expiresAt *time.Time, grantedBy string) error
	SetUserManualEntitlementKind(userID, kind string, expiresAt *time.Time, grantedBy string) error
	ListUserBillingByStatus(status string) ([]UserBilling, error)
	ListUserBilling() ([]UserBilling, error)
	SetUserOverLimitSince(userID string, at *time.Time) error
	ListServersByOwner(ownerID string) ([]models.Server, error)
	ListBackupRunsByOwner(ownerID string) ([]BackupRunRef, error)
	// BackupBytesByOwner is what the tenant stores ON OURS, which is what the
	// quota is about. Archives on a bucket they connected themselves are
	// counted separately, because we do not pay for those.
	BackupBytesByOwner(ownerID string) (int64, error)
	BackupBytesByOwnerOnOwnStorage(ownerID string) (int64, error)
	// --- BYON plans + limits ---
	ListPlans() ([]Plan, error)
	GetPlan(id int) (*Plan, error)
	GetDefaultPlan() (*Plan, error)
	CreatePlan(p Plan) (int, error)
	UpdatePlan(p Plan) error
	DeletePlan(id int) error
	GetUserPlanID(userID string) (*int, error)
	SetUserPlan(userID string, planID *int) error
	SetUserLimitOverrides(userID string, maxNodes, maxLinks, trafficEdge, trafficRelay, trafficCombined *int64) error
	SetUserPurchasedEntitlement(userID string, maxNodes *int64, setNodes bool, maxLinks *int64, setLinks bool) error
	SetUserTrafficBilling(userID string, ceilingGB int64, enabled bool) error
	CountNodesByOwner(ownerID string) (int, error)
	CountLinkKitsByOwner(ownerID string) (int, error)
	CountNodeWarpKeysByOwner(ownerID string) (int, error)
	SetNodeLastSeen(id int) error
	SetNodePlacement(id int, cpuRatio, ramRatio float64) error
	UpdateNodeCapacity(id int, totalCPU float64, totalRAMMB int64) error
	SetNodeRegion(id int, region string) error
	// SetNodeConfig persists an admin's panel-configured name/region/tags and
	// marks the node configured=true so the heartbeat env stops overwriting them.
	SetNodeConfig(id int, name, region, tags string) error
	SumAllocatedByNode(nodeID int) (totalRAMMB int64, totalCPU float64, err error)
	// ServerDiskLimitsByNode returns uuid -> disk limit in MB for every server
	// on the node, including servers whose limit is 0 (unlimited). Used to sum
	// how much disk a storage path has already PROMISED, which free space alone
	// cannot show.
	ServerDiskLimitsByNode(nodeID int) (map[string]int64, error)
	CountServersByNode(nodeID int) (int, error)
	ListServersByNode(nodeID int) ([]models.Server, error)
	DeleteServersByNode(nodeID int) error
	DeleteStaleOfflineNodes(offlineSince time.Time) (int, error)

	// --- Servers ---
	CreateServer(srv *models.Server) (int64, error)
	ListServers(filterByUser string) ([]models.Server, error)
	GetServerByID(id int) (*models.Server, error)
	GetServerByUUID(uuid string) (*models.Server, error)
	DeleteServer(id int) error
	UpdateServerStatus(id int, status string) error
	UpdateServerDesiredState(id int, desiredState string) error
	UpdateServerSetup(id int, image, command, activeSubServer, extraJvmFlags, installerType, minecraftVersion, buildNumber string) error
	// UpdateServerLoaderMetadata persists ONLY installer_type/minecraft_version/
	// build_number - the "declare metadata" path for an imported server. Unlike
	// UpdateServerSetup it never touches game_image/start_command/
	// active_sub_server/extra_jvm_flags and the caller never dispatches an
	// install/reinstall command, so this cannot trigger a reinstall.
	UpdateServerLoaderMetadata(id int, installerType, minecraftVersion, buildNumber string) error
	UpdateServerActiveSubServer(id int, subServer string) error
	UpdateServerName(id int, name string) error
	UpdateServerResources(id int, ram int, cpuLimit float64, diskLimit int64) error
	UpdateServerCPUPinning(id int, mode, cpuset string) error
	ListServerCpusetsByNode(nodeID int) (map[int]string, error)
	ResetServerCPUPinningByNode(nodeID int) (int64, error)
	UpdateServerPorts(id int, hostPort, containerPort int) error
	GetUsedHostPortsOnNode(nodeID int) ([]int, error)
	GetAllActiveServers() ([]models.Server, error)
	CountServersByOwner(ownerID string) (int, error)
	UpdateServerProxyID(id int, proxyID *int) error
	UpdateServerOwner(id int, ownerID *string) error
	SetServerAutoMove(id int, enabled bool) error
	// UpdateServerNode reassigns a server to a different node. Used by the
	// auto-move migration flow (later wave) to flip the FK after transport.
	UpdateServerNode(serverID int, newNodeID int) error
	// ResetAllAutoMove clears the auto_move opt-in on every server. Called when
	// gateway routing is switched off, since auto-move cannot run without it.
	ResetAllAutoMove() error

	// --- Server Invites ---
	CreateInvite(serverID int, userID, invitedBy string, permissions map[string]bool) error
	DeleteInvite(serverID int, userID string) error
	UpdateInvitePermissions(serverID int, userID string, permissions map[string]bool) error
	GetInvite(serverID int, userID string) (*models.ServerInvite, error)
	ListInvitesByServer(serverID int) ([]models.ServerInvite, error)
	CountInvitesPerServer() (map[int]int, error)
	ListServersForUser(userID string, isAdmin bool) ([]models.Server, error)

	// --- Authz (permission-system foundation, phase 1; additive) ---
	// Read-side accessors the authz.Resolver depends on. Write-side CRUD
	// (create/update panel + server roles, reworked invites) lands in phases
	// 3-4. GetServerGrant/GetAccountGrant read the reworked server_invites
	// columns; the legacy GetInvite path stays intact for existing callers.
	GetPanelRole(id int) (*PanelRole, error)
	GetServerRole(id int) (*ServerRole, error)
	GetUserPanelAuthz(userID string) (*int, CapOverrides, error)
	GetServerGrant(serverID int, userID string) (*ServerGrant, error)
	GetAccountGrant(ownerUserID, userID string) (*ServerGrant, error)

	// Write-side panel-role CRUD + per-user assignment (phase 2). Server-role
	// + reworked-invite writes land in phase 4. Capability validation against
	// the catalog happens in the handler (the store package must not import
	// authz - authz imports store).
	CreatePanelRole(name string, capabilities []string, createdBy *string) (int, error)
	ListPanelRoles() ([]PanelRole, error)
	UpdatePanelRole(id int, name string, capabilities []string) error
	DeletePanelRole(id int) error
	SetUserPanelRole(userID string, roleID *int) error
	SetUserPanelCapOverrides(userID string, ov CapOverrides) error

	// Write-side owner-scoped server-role CRUD (phase 3). Capability validation
	// against the catalog happens in the handler (the store must not import
	// authz). Update/Delete are owner-scoped so a user only touches their realm.
	CreateServerRole(ownerUserID, name string, capabilities []string) (int, error)
	ListServerRolesByOwner(ownerUserID string) ([]ServerRole, error)
	UpdateServerRole(id int, ownerUserID, name string, capabilities []string) error
	DeleteServerRole(id int, ownerUserID string) error

	// Write-side reworked-invite grant upsert/delete (phase 3). serverID nil =
	// account-wide grant (relies on the F6 partial unique index). Capability +
	// delegation-cap checks are the handler's job.
	UpsertServerGrant(serverID *int, userID, ownerUserID string, serverRoleID *int, overrides CapOverrides, inherit bool) error
	DeleteServerGrant(serverID *int, ownerUserID, userID string) error
	ListGrantsByOwner(ownerUserID string) ([]OwnerGrant, error)

	// --- Backups ---
	// ListBackupStorages returns the PLATFORM's own storages only. A tenant's
	// private bucket must never appear in an admin dropdown as a target for
	// somebody else's backups; ListBackupStoragesByOwner is the other half.
	ListBackupStorages() ([]models.BackupStorage, error)
	ListBackupStoragesByOwner(ownerID string) ([]models.BackupStorage, error)
	GetBackupStorage(id int) (*models.BackupStorage, error)
	GetDefaultBackupStorage() (*models.BackupStorage, error)
	// GetUserDefaultBackupStorage is the middle step of the resolution chain.
	// nil is not an error: it means "this tenant has not set one, ask the
	// platform".
	GetUserDefaultBackupStorage(ownerID string) (*models.BackupStorage, error)
	CreateBackupStorage(s *models.BackupStorage) (int, error)
	UpdateBackupStorage(s *models.BackupStorage) error
	DeleteBackupStorage(id int) error

	ListBackupJobs(serverID int) ([]models.BackupJob, error)
	GetBackupJob(id int) (*models.BackupJob, error)
	CreateBackupJob(j *models.BackupJob) (int, error)
	UpdateBackupJob(j *models.BackupJob) error
	DeleteBackupJob(id int) error
	ListDueBackupJobs(now time.Time) ([]models.BackupJob, error)
	SetBackupJobScheduled(jobID int, lastRun, nextRun time.Time) error

	ListBackupRuns(jobID int, limit int) ([]models.BackupRun, error)
	GetBackupRun(id int) (*models.BackupRun, error)
	CreateBackupRun(r *models.BackupRun) (int, error)
	UpdateBackupRunStatus(id int, status, errorMsg string, sizeBytes int64, storageKey string, completed time.Time) error
	DeleteBackupRun(id int) error
	PruneOldBackupRuns(jobID, keep int) ([]models.BackupRun, error)
	ListAbandonedBackupRuns(startedBefore time.Time, limit int) ([]models.BackupRun, error)

	CreateBackupRestore(r *models.BackupRestore) (int, error)
	GetBackupRestore(id int) (*models.BackupRestore, error)
	ListBackupRestores(serverID, limit int) ([]models.BackupRestore, error)
	UpdateBackupRestoreStatus(id int, status, errorMsg string, completed time.Time) error

	// --- Storage connections ---
	ListStorageConnections() ([]models.StorageConnection, error)
	GetStorageConnection(id int) (*models.StorageConnection, error)
	CreateStorageConnection(c *models.StorageConnection) (int, error)
	UpdateStorageConnection(c *models.StorageConnection) error
	SetStorageConnectionSecret(id int, secret string) error
	DeleteStorageConnection(id int) error

	// --- Storage migration manifests ---
	CreateStorageManifest(m *models.StorageManifest, entries []models.StorageManifestEntry) (int, error)
	GetStorageManifest(id int) (*models.StorageManifest, error)
	ListStorageManifests(dataSet string, limit int) ([]models.StorageManifest, error)
	ListStorageManifestEntries(manifestID int) ([]models.StorageManifestEntry, error)
	DeleteStorageManifest(id int) error
	// ListModpackStorageKeys returns the union of modversions.storage_key,
	// pack_builds.mrpack_storage_key and loaders.client_storage_key. It is the
	// ONLY way to enumerate the modpacks key space: ModpackStorageProvider has
	// no List.
	ListModpackStorageKeys() ([]string, error)
	// ListModversionSHA512ByStorageKey maps storage_key -> the third-party
	// SHA-512 already recorded for that mod file. Opportunistic cross-check
	// input only; never a manifest source of truth.
	ListModversionSHA512ByStorageKey() (map[string]string, error)

	// --- Modules ---
	ListModules() ([]models.Module, error)
	GetModuleByID(id int) (*models.Module, error)
	CreateModule(mod *models.Module) (int, error)
	DeleteModule(id int) error
	UpdateModuleStatus(id int, isEnabled bool) error
	UpdateModulePosition(id int, position int) error
	SetModuleAccessRole(id int, role string) error

	// --- Settings ---
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error

	// --- Health ---
	// Ping verifies the underlying database connection is alive. Backed by
	// sql.DB.PingContext so the status endpoint can report DB liveness without
	// running a real query.
	Ping(ctx context.Context) error
	// TimescaleEnabled reports whether the TimescaleDB extension is installed.
	// server_stats is created as a hypertable only when the extension is present;
	// without it the table still works as plain Postgres but loses automatic
	// retention and fast long-range history queries, which the status endpoint
	// surfaces as a degraded (not failed) component.
	TimescaleEnabled(ctx context.Context) (bool, error)
	// IsServerStatsHypertable reports whether server_stats is already a TimescaleDB
	// hypertable. Only meaningful when the timescaledb extension is installed.
	IsServerStatsHypertable(ctx context.Context) (bool, error)
	// ConvertServerStatsToHypertable promotes the existing plain server_stats table
	// to a hypertable IN PLACE (migrate_data) and (re)applies the retention policy.
	// Used after swapping a plain-Postgres image for a TimescaleDB one on the same DB.
	ConvertServerStatsToHypertable(ctx context.Context) error
	// EstimateServerStatsRows returns the planner's row-count estimate for
	// server_stats (pg_class.reltuples) - instant, unlike COUNT(*) on a huge table.
	EstimateServerStatsRows(ctx context.Context) (int64, error)

	// --- Warp ---
	CreateWarpAPIKey(k WarpAPIKey) (int, error)
	GetWarpAPIKeyByHash(hash string) (*WarpAPIKey, error)
	ListWarpAPIKeysByOwner(ownerID string) ([]WarpAPIKey, error)
	ListWarpAPIKeys() ([]WarpAPIKey, error)
	GetWarpAPIKeyByID(id int) (*WarpAPIKey, error)
	RevokeWarpAPIKeyByID(id int) error
	DeleteWarpAPIKeyByID(id int) error
	// ListLinkKitsForACLReconcile returns non-revoked route-only link kits whose
	// owner is NOT hard-suspended (owner suspended with suspended_at <=
	// hardSuspendedBefore is excluded; NULL-owner admin kits are always included).
	ListLinkKitsForACLReconcile(hardSuspendedBefore, overLimitBefore time.Time) ([]WarpAPIKey, error)
	// ListLinkKitsForACLTeardown returns link kits that must NOT have an ACL
	// right now (revoked recently, or owner hard-suspended past grace) - the
	// self-heal counterpart to ListLinkKitsForACLReconcile, feeding the
	// reconciler's cleanup sweep.
	ListLinkKitsForACLTeardown(hardSuspendedBefore, overLimitBefore, revokedAfter time.Time) ([]WarpAPIKey, error)
	GetWarpAPIKeyByNodeID(nodeID string) (*WarpAPIKey, error)
	RevokeWarpAPIKeyByNodeID(nodeID string) error
	RollWarpAPIKeyHash(nodeID, newHash string) error
	InsertWarpPeer(p WarpPeer) (int, error)
	GetWarpPeerByPubkey(pubkey string) (*WarpPeer, error)
	ListWarpPeersByKey(apiKeyID int) ([]WarpPeer, error)
	ListAllWarpPeers() ([]WarpPeer, error)
	// Custom-domain ownership proof. Keyed on (user, domain) so a block can
	// never be global - see database/db_custom_domains.go.
	GetCustomDomainClaim(userID, domain string) (*CustomDomainClaim, error)
	StartCustomDomainClaim(userID, domain string, deadline time.Time) (*CustomDomainClaim, error)
	MarkCustomDomainVerified(id int) error
	FailCustomDomainClaim(id int) (string, error)
	ListExpiredPendingClaims(now time.Time) ([]CustomDomainClaim, error)
	ListPendingClaims() ([]CustomDomainClaim, error)
	ListCustomDomainClaimsByUser(userID string) ([]CustomDomainClaim, error)
	SetCustomDomainTXTToken(id int, token string) error

	ListWarpPeersByRegion(region string) ([]WarpPeer, error)
	SetWarpPeerAssignedLeader(pubkey, leaderID string) error
	CountWarpPeersByRegion() (map[string]int, error)
	DeleteWarpPeerByPubkey(pubkey string) error
	// Warp regions + leaders (multi-hub: region = identity, leaders = endpoints)
	ListWarpRegions() ([]WarpRegion, error)
	GetWarpRegion(region string) (*WarpRegion, error)
	UpsertWarpRegion(region, subnet string, enabled bool) error
	DeleteWarpRegion(region string) error
	ListWarpLeaders() ([]WarpLeader, error)

	// Route-only routes. The durable side of route:<domain> in Redis - see
	// CoreLinkRoute.
	UpsertCoreLinkRoute(r CoreLinkRoute) error
	ListCoreLinkRoutes() ([]CoreLinkRoute, error)
	DeleteCoreLinkRoute(domain string) error
	ListWarpLeadersByRegion(region string) ([]WarpLeader, error)
	UpsertWarpLeader(leaderID, region, endpoint string, enabled bool) error
	DeleteWarpLeader(leaderID string) error
	SeedWarpRegionIfEmpty(region, subnet, leaderID, endpoint string) error

	// --- SFTP ---
	GetSFTPAccessByNode(nodeID int) ([]SFTPAccess, error)

	// --- Stats ---
	InsertStatsBatch(stats []models.ServerStatRow) error
	GetStatsHistory(serverUUID string, since time.Time) ([]models.ServerStatRow, error)
	InsertGatewayBandwidthBatch(rows []models.GatewayBandwidthRow) error
	GetGatewayBandwidthHistory(since time.Time, component, host string) ([]models.GatewayBandwidthRow, error)

	// --- Library Disabled Paths ---
	ListDisabledLibraryPaths() ([]string, error)
	SetLibraryPathDisabled(path string, disabled bool) error

	// --- Traffic limits: what a tenant may use and buy, per (region, kind) ---
	// Same three-scope shape as the route limits below. nil from Get means the
	// scope says nothing, not that there is no limit - see
	// services.ResolveTrafficLimit.
	GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error)
	SetTrafficLimit(scope, region, kind string, includedGB, maxPurchaseGB *int64) error
	ListTrafficLimits() ([]models.TrafficLimit, error)
	DeleteTrafficLimit(scope, region, kind string) error

	// --- Gateway Route Limits (still managed by Core, not Hub) ---
	GetGatewayRouteLimit(scope string) (*models.GatewayRouteLimit, error)
	SetGatewayRouteLimit(scope string, max *int) error
	ListGatewayRouteLimits() ([]models.GatewayRouteLimit, error)
	DeleteGatewayRouteLimit(scope string) error

	// --- Regions ---
	ListRegions(includeDisabled bool) ([]models.Region, error)
	GetRegion(id string) (*models.Region, error)
	CreateRegion(r *models.Region) error
	UpdateRegion(r *models.Region) error
	DeleteRegion(id string) error
	CountServersInRegion(regionID string) (int, error)
	CountNodesInRegion(regionID string) (int, error)

	// --- User <-> Region M:N ---
	GetUserRegionIDs(userID string) ([]string, error)
	SetUserRegions(userID string, allAccess bool, regionIDs []string) error
	SetUserAllRegionsAccess(userID string, allAccess bool) error
	GetUserAllRegionsAccess(userID string) (bool, error)

	// --- Identity Audit Log (append-only) ---
	InsertAuditIdentity(ev *models.AuditEventIdentity) error
	ListAuditIdentity(targetUserID *string, eventType string, limit int) ([]models.AuditEventIdentity, error)

	// --- Settings audit trail ---
	// SetSettingBy stores a setting value plus the user who changed it. Existing
	// callers can keep using SetSetting (updated_by = NULL) — only new flows
	// that care about audit need this variant.
	SetSettingBy(key, value string, updatedBy string) error

	// --- Email verification + login tracking ---
	GetUserByEmail(email string) (*models.User, error)
	GetUserByEmailVerificationToken(token string) (*models.User, error)
	SetEmailVerificationToken(userID string, token string) error
	MarkEmailVerified(userID string) error
	// SetUserEmail also clears the verification state; see the implementation
	// for why the two are one operation.
	SetUserEmail(userID, email string) error
	UpdateLastLoginAt(userID string) error

	// --- Password reset ---
	GetUserByPasswordResetToken(token string) (*models.User, error)
	SetPasswordResetToken(userID string, token string, expiresAt, issueWhenExpiryAtOrBefore time.Time) (bool, error)
	ClearPasswordResetToken(userID string) error

	// --- Security questions ---
	// GetUserSecurityQuestions returns the question texts only (no hashes),
	// in the same order they were stored — the verify path matches answers
	// positionally so order is part of the contract.
	GetUserSecurityQuestions(userID string) ([]string, error)
	// SetUserSecurityQuestions replaces the user's whole list. Pass an empty
	// slice to clear (allowed when the policy lets users opt out).
	SetUserSecurityQuestions(userID string, qaJSON string) error
	// GetUserSecurityQuestionsRaw returns the stored JSON for verification
	// — caller bcrypt-compares answer-by-answer.
	GetUserSecurityQuestionsRaw(userID string) (string, error)
	CountUsersMissingSecurityQuestions() (missing int, total int, err error)

	// --- Roles + granular permissions ---
	// SetUserRole writes both role and the legacy is_admin flag so handlers
	// that still read is_admin stay in sync. Valid roles: 'user', 'support', 'admin'.
	SetUserRole(userID string, role string) error
	// SetUserPermissionFlags sets the can_* booleans. SupportTeam is optional;
	// pass "" to clear.
	SetUserPermissionFlags(userID string, canDeleteServers, canChangeResources bool, supportTeam string) error

	// --- Tickets ---
	// Categories
	ListTicketCategories(includeDisabled bool) ([]models.TicketCategory, error)
	GetTicketCategory(id int) (*models.TicketCategory, error)
	CreateTicketCategory(c *models.TicketCategory) (int, error)
	UpdateTicketCategory(c *models.TicketCategory) error
	DeleteTicketCategory(id int) error

	// Tickets
	CreateTicket(t *models.Ticket) (int, error)
	GetTicket(id int) (*models.Ticket, error)
	// ListTickets supports the user list (filter user_id), the support inbox
	// (status, assignedUserID, assignedTeam filters), and admin global view.
	// Pass empty/nil for filters you don't want applied. limit caps at 200.
	ListTickets(filter TicketFilter) ([]models.Ticket, error)
	UpdateTicketStatus(id int, status string) error
	UpdateTicketPriority(id int, priority string) error
	UpdateTicketAssignment(id int, assignedUserID *string, assignedTeam string) error
	TouchTicketUpdated(id int) error // bump updated_at — call after any mutation

	// Messages
	AddTicketMessage(m *models.TicketMessage) (int, error)
	ListTicketMessages(ticketID int, includeInternal bool) ([]models.TicketMessage, error)

	// Watchers
	ListTicketWatchers(ticketID int) ([]models.TicketWatcher, error)
	AddTicketWatcher(w *models.TicketWatcher) error
	RemoveTicketWatcher(ticketID int, userID string) (removed bool, err error)
	IsTicketWatcher(ticketID int, userID string) (bool, error)

	// Audit
	InsertTicketAudit(ev *models.TicketAuditEvent) error
	ListTicketAudit(ticketID int) ([]models.TicketAuditEvent, error)
	// PurgeTicketAuditOlderThan backs the retention sweep, mirroring
	// PurgeServerAuditOlderThan.
	PurgeTicketAuditOlderThan(cutoff time.Time) (int, error)

	// Sidebar: servers attached to active tickets assigned to a support user.
	ListServersViaActiveTickets(supportUserID string) ([]models.Server, error)

	// --- Ticket deletion (admin-only, audited) ---
	// DeleteTicket removes the ticket + every attached row in one tx. Returns
	// sql.ErrNoRows when the ticket id doesn't exist.
	DeleteTicket(id int) error
	// InsertTicketDeletion stamps the audit row. The handler builds the
	// snapshot fields before calling DeleteTicket so the row is independent of
	// the now-gone source data.
	InsertTicketDeletion(rec *models.TicketDeletion) error
	// ListTicketDeletions returns (rows, total) for the admin Deletion-Log UI.
	// deletedBy "" means no filter; limit is clamped server-side.
	ListTicketDeletions(limit, offset int, deletedBy string) ([]models.TicketDeletion, int, error)
	// ListAttachmentStorageKeysByTicket returns the on-disk storage keys for a
	// ticket so the handler can delete the file blobs after the DB rows are
	// gone. Called BEFORE DeleteTicket so the keys are still readable.
	ListAttachmentStorageKeysByTicket(ticketID int) ([]string, error)

	// --- Tickets (attachments, canned responses, notifications) ---
	// Attachments
	AddTicketAttachment(a *models.TicketAttachment) (int, error)
	GetTicketAttachment(id int) (*models.TicketAttachment, error)
	ListTicketAttachments(ticketID int) ([]models.TicketAttachment, error)
	DeleteTicketAttachment(id int) error
	SumAttachmentBytesByTicket(ticketID int) (int64, error)
	SumAttachmentBytesByUser(userID string) (int64, error)

	// Canned responses
	ListCannedResponses(categoryID *int) ([]models.CannedResponse, error)
	GetCannedResponse(id int) (*models.CannedResponse, error)
	CreateCannedResponse(c *models.CannedResponse) (int, error)
	UpdateCannedResponse(c *models.CannedResponse) error
	DeleteCannedResponse(id int) error

	// Notifications
	InsertNotification(n *models.Notification) (int64, error)
	ListNotifications(userID string, includeRead bool, limit int) ([]models.Notification, error)
	CountUnreadNotifications(userID string) (int, error)
	MarkNotificationRead(id int64, userID string) error
	MarkAllNotificationsRead(userID string) error

	// Auto-close support
	ListResolvedTicketsOlderThan(cutoff time.Time) ([]int, error)
	// Watchers + assignee lookup for notification fan-out
	ListTicketParticipantsForNotify(ticketID int, excludeUserID string) ([]string, error)

	// --- Migration + backup raw access ---
	// CountTicketRows returns the row count for a single ticket-related
	// table from the main DB. Used by the dry-run + status endpoints
	// without dragging full per-table SELECTs into the migration handler.
	CountTicketRows(table string) (int, error)
	// DumpTicketTable streams all rows of the named table as
	// []map[string]interface{}. Used by both backup and migration.
	DumpTicketTable(table string) ([]map[string]interface{}, error)

	// --- Server audit ---
	InsertServerAudit(ev *models.ServerAuditEvent) error
	ListServerAudit(serverID int, eventType string, limit, offset int) ([]models.ServerAuditEvent, int, error)
	SetServerAuditEnabled(serverID int, enabled bool) error
	SetServerAuditForceOn(serverID int, force bool) error
	// GetServerAuditState returns the (enabled, forceOn, count) tuple in one
	// query so the status endpoint stays cheap.
	GetServerAuditState(serverID int) (enabled, forceOn bool, count int, err error)
	// PurgeServerAuditOlderThan supports the retention sweep service.
	PurgeServerAuditOlderThan(cutoff time.Time) (int, error)

	// --- Auto-delete inactive users ---
	// ListInactiveCandidates returns active non-admin users whose last login
	// (or creation when never logged in) is older than `idleSince`. The
	// HasHistory flag lets the calling job apply an extra grace window
	// without a second query.
	ListInactiveCandidates(idleSince time.Time) ([]InactiveCandidate, error)
	// MarkUserPendingDeletion stages the user; the warning mail is sent
	// separately by the job so a mail failure doesn't roll back the stamp.
	MarkUserPendingDeletion(userID string, scheduledAt time.Time) error
	// ListUsersDueForDeletion returns user IDs whose scheduled_at is <= now
	// AND who are still in pending_deletion state.
	ListUsersDueForDeletion(now time.Time) ([]string, error)
	// CancelUserDeletion clears warning/scheduled stamps and resets status.
	// Idempotent: safe to call on already-active users.
	CancelUserDeletion(userID string) error
	// AnonymizeUser wipes PII (username/email/password/2FA/security questions)
	// but keeps the row + id so audit references stay valid.
	AnonymizeUser(userID string) error

	// --- Scheduled Tasks ---
	// Per-server cron jobs. NextRun is computed on insert/update from the cron
	// string + now() and re-computed by the leader-gated executor after each
	// run. ListDueScheduledTasks is the executor's hot path; the partial
	// index in applyPhase8Schema keeps it cheap.
	ListScheduledTasksByServer(serverID int) ([]models.ScheduledTask, error)
	GetScheduledTask(id int) (*models.ScheduledTask, error)
	CreateScheduledTask(t *models.ScheduledTask) (int, error)
	UpdateScheduledTask(t *models.ScheduledTask) error
	DeleteScheduledTask(id int) error
	SetScheduledTaskEnabled(id int, enabled bool, nextRun *time.Time) error
	ListDueScheduledTasks(now time.Time, limit int) ([]models.ScheduledTask, error)
	RecordScheduledTaskRun(id int, ranAt time.Time, status, errMsg string, nextRun *time.Time) error

	// --- RCON config ---
	// Lazy accessors so RCON fields don't bloat every server-list scan.
	// Password is stored in plaintext for V1 — the column is excluded from
	// any list query and only fetched by the RCON-exec path.
	GetServerRconConfig(serverID int) (enabled bool, port int, password string, err error)
	SetServerRconConfig(serverID int, enabled bool, port int, password string) error

	// rcon_needs_restart tracks whether a server.properties RCON change is still
	// waiting on a (re)start. MC only reads RCON settings at JVM start, so the
	// change is inert until then. Persisting it lets the panel restore the
	// "restart required" banner and keep the RCON-dependent Players tabs locked
	// across a reload, instead of losing that state to client-only React state.
	// Set on SetServerRconConfig writes, cleared when the server (re)starts.
	GetServerRconNeedsRestart(serverID int) (bool, error)
	SetServerRconNeedsRestart(serverID int, needsRestart bool) error

	// rcon_log_filter is the per-server console toggle that drops Minecraft's
	// per-RCON-connection thread chatter from the log stream. Same lazy-accessor
	// shape as the two above; ListServerRconLogFilter is the bulk read the
	// periodic Redis republish uses (the log-shipper reads the live value from
	// Redis so the toggle applies without recreating the container).
	GetServerRconLogFilter(serverID int) (bool, error)
	SetServerRconLogFilter(serverID int, on bool) error
	ListServerRconLogFilter() ([]ServerRconLogFilter, error)

	// --- Edge transitional MOTD (per-server) ---
	// Lazy accessors (same rationale as RCON): the two columns stay off the
	// giant server-list scans. ListServerEdgeMotd is a single bulk query used
	// by the periodic Redis republish loop (one query, not one per server).
	GetServerEdgeMotd(serverID int) (mode, text string, err error)
	SetServerEdgeMotd(serverID int, mode, text string) error
	ListServerEdgeMotd() ([]ServerEdgeMotd, error)

	// --- API Keys ---
	// Public RCON surface backing. Plaintext key is shown to the user once
	// on creation; the DB only ever sees sha256 hash. Scope JSON shape:
	//   { "servers": ["uuid-1", ...], "permissions": ["rcon.exec"] }
	CreateAPIKey(k *models.APIKey) (int, error)
	ListAPIKeysByUser(userID string) ([]models.APIKey, error)
	GetAPIKeyByHash(hash string) (*models.APIKey, error)
	RevokeAPIKey(id int, userID string) error
	TouchAPIKey(id int) error

	// --- Installed Mods ---
	// Per-server-per-sub-server inventory of Modrinth-sourced mods/plugins.
	// Drives the "Installed" view + the lazy update-detection scan.
	UpsertServerMod(m *models.ServerMod) (int, error)
	ListServerMods(serverID int, subServerName string) ([]models.ServerMod, error)
	// ListServerModSubServers lists sub-servers that HAVE mod rows, which is not
	// the same set as the ones with install records.
	ListServerModSubServers(serverID int) ([]string, error)
	// ReplaceServerMods makes the rows for one sub-server exactly mods, in one
	// transaction. An empty slice clears the scope, which is a restore saying
	// "there were none" rather than saying nothing.
	ReplaceServerMods(serverID int, subServerName string, mods []models.ServerMod) error
	DeleteServerMod(id, serverID int) error
	// GetServerModByProject is what an install has to read BEFORE its own
	// upsert: that upsert overwrites file_name, and the overwritten value is
	// the name of the jar the old version left behind.
	GetServerModByProject(serverID int, subServerName, projectID string) (*models.ServerMod, error)
	// SetServerModStatus applies a node's report. Reports false when the row
	// has moved on to another attempt, which is a late answer rather than an
	// error.
	SetServerModStatus(serverID int, subServerName, projectID, installID, status, message string) (bool, error)
	// RecordServerModHistory files the version an install replaces, keeping the
	// newest three per project so a bad update can be undone.
	RecordServerModHistory(serverID int, subServerName string, prev *models.ServerMod) error
	ListServerModHistory(serverID int, subServerName string) ([]models.ServerModHistoryEntry, error)

	// SetUserBillingConsent records what a tenant agreed to be charged for.
	// A nil argument leaves that flag alone.
	SetUserBillingConsent(userID string, traffic, backup *bool) error

	// UpdateServerRuntime changes ONLY the Java image and the JVM flags, for a
	// settings edit that must not rewrite the installer metadata alongside them.
	UpdateServerRuntime(serverID int, javaImage, extraJvmFlags string) error

	// SetBackupRunInstallSnapshot records how the archived sub-servers were
	// installed, so a restore can put those records back.
	SetBackupRunInstallSnapshot(runID int, snapshot string) error
	// SetBackupRunManifest records the archive's own description, the same bytes
	// the node writes into the archive.
	SetBackupRunManifest(runID int, manifest string) error

	// --- Sub-server installs ---
	// How each sub-server was installed, per (server, sub-server): installer,
	// versions, and the exact modpack or pack it came from. Written on every
	// install; read by the panel so an edit can put the same choices back on
	// screen instead of asking the operator to remember them.
	UpsertSubServerInstall(in models.SubServerInstall) error
	GetSubServerInstall(serverID int, subServer string) (*models.SubServerInstall, error)
	ListSubServerInstalls(serverID int) ([]models.SubServerInstall, error)
	DeleteSubServerInstall(serverID int, subServer string) error

	// --- Modpack contents snapshot ---
	// Per-server-per-sub-server snapshot of the modpack's Modrinth-identified
	// members, captured at install/reinstall. Backs the advisory Content-tab
	// cross-check (client-side). Cleared + rewritten on each (re)install.
	ReplaceServerModpackContents(serverID int, subServer string, rows []models.ServerModpackContent) error
	ListServerModpackContents(serverID int, subServer string) ([]models.ServerModpackContent, error)

	// Modrinth PATs. One row per user; SetModrinthPAT upserts and
	// stamps last_validated_at on success. ClearModrinthPAT removes the
	// row entirely so a revoked PAT can't accidentally be re-used.
	SetModrinthPAT(userID string, ciphertext, modrinthUsername string) error
	GetModrinthPAT(userID string) (*models.ModrinthPAT, error)
	ClearModrinthPAT(userID string) error

	// --- Unified packs (Solder + Modrinth) ---
	CreatePack(p *models.Pack) (int, error)
	UpdatePack(p *models.Pack) error
	DeletePack(id int, ownerID string) error
	GetPack(id int) (*models.Pack, error)
	ListPacksByOwner(ownerID string) ([]models.Pack, error)
	CreatePackBuild(b *models.PackBuild) (int, error)
	UpdatePackBuild(b *models.PackBuild) error
	DeletePackBuild(id, packID int) error
	GetPackBuild(id int) (*models.PackBuild, error)
	ListPackBuilds(packID int) ([]models.PackBuild, error)
	// Public Solder lookups (addressed by SolderSlug + version string, not numeric ID).
	GetPackBySolderSlug(slug string) (*models.Pack, error)
	// Per-account Solder addressing. Slugs are unique per OWNER, so a bare slug
	// is only an address once the account is known.
	GetPackBySolderSlugForOwner(ownerID, slug string) (*models.Pack, error)
	ListPublicSolderPacksByOwner(ownerID string) ([]models.Pack, error)
	GetUserIDBySolderHandle(handle string) (string, error)
	GetSolderHandle(userID string) (string, error)
	SetSolderHandle(userID, handle string) error
	GetPackBuildByVersion(packID int, versionString string) (*models.PackBuild, error)
	ListSolderPublishedBuilds(packID int) ([]models.PackBuild, error)
	ListPublicSolderPacks() ([]models.Pack, error)
	// CountPrivateSolderPacks counts Solder-capable packs (have a solder_slug)
	// that are private or hidden - used to warn before enabling public delivery.
	CountPrivateSolderPacks() (int, error)
	// AnyPublishedSolderModKey returns one storage key reachable through a
	// published Solder build, or "" when there is none - the public-delivery
	// probe needs a real object to ask a definitive question.
	AnyPublishedSolderModKey() (string, error)
	// Access-controlled Solder pack listings (Phase 3c).
	ListAllSolderPacks(ownerID string) ([]models.Pack, error)
	ListSolderPacksForClient(clientID int) ([]models.Pack, error)

	// Solder clients (per-owner Technic Launcher identities for pack whitelisting).
	CreateSolderClient(name, ownerID string) (*SolderClient, error)
	ListSolderClientsByOwner(ownerID string) ([]SolderClient, error)
	GetSolderClient(id int, ownerID string) (*SolderClient, error)
	DeleteSolderClient(id int, ownerID string) error
	GetSolderClientByUUID(uuid string) (*SolderClient, error)

	// Pack-client whitelist.
	AddPackClient(packID, clientID int) error
	RemovePackClient(packID, clientID int) error
	ListPackClients(packID int) ([]SolderClient, error)
	IsPackClient(packID, clientID int) (bool, error)

	// Solder keys (global API keys; only the sha256 hash is stored).
	CreateSolderKey(name, ownerID, keyHash string) (*SolderKey, error)
	ListSolderKeysByOwner(ownerID string) ([]SolderKey, error)
	DeleteSolderKey(id int, ownerID string) error
	GetSolderKeyByHash(keyHash string) (*SolderKey, error)

	// --- Share links (tokenized build download links) ---
	CreateShareLink(l *models.ShareLink) (int, error)
	GetShareLinkByToken(token string) (*models.ShareLink, error)
	ListShareLinksByBuild(buildID int) ([]models.ShareLink, error)
	RevokeShareLink(id int, createdBy string) error

	GetLoader(minecraft, loader, loaderVersion string) (*models.Loader, error)
	UpsertLoader(l *models.Loader) (int, error)
	UpdateLoaderStatus(minecraft, loader, loaderVersion, status, buildError string) error

	UpsertMod(m *models.Mod) (int, error)
	GetModBySlug(ownerID, slug string) (*models.Mod, error)
	CreateModversion(mv *models.Modversion) (int, error)
	UpdateModversion(mv *models.Modversion) error
	GetModversion(id int) (*models.Modversion, error)
	FindModversionBySHA1(ownerID, sha1 string) (*models.Modversion, error)
	// CountModversionsByStorageKey guards a storage delete: an object can have
	// more than one modversion row pointing at it (see copyUploadedContent).
	CountModversionsByStorageKey(key string) (int, error)
	AttachModversionToBuild(buildID, modversionID int, side string) (int, error)
	DetachFromBuild(buildID, modversionID int) error
	IsModversionInBuild(buildID, modversionID int) (bool, error)
	ListBuildContent(buildID int) ([]models.BuildContentEntry, error)
	ListModversionsDueForCheck(before time.Time) ([]ModversionCheckRow, error)
	SetModversionCheckResult(id int, latestVersionID string, checkedAt time.Time) error

	// --- Username history + admin rename ---
	RenameUser(userID string, newUsername string, changedBy string) error
	ListUsernameHistory(userID string) ([]models.UsernameHistory, error)
	GetUserAccountPolicy() (allowChange bool, cooldownDays int, err error)
	SetUserAccountPolicy(allowChange bool, cooldownDays int) error

	// --- Per-user feature flag ---
	SetUserCanCreateModpacks(userID string, can bool) error
	// ClearUserCanCreateModpacksManual puts a user back under the platform
	// authoring toggle by dropping the manual marker, leaving the value alone.
	ClearUserCanCreateModpacksManual(userID string) error
	// BulkSetCanCreateModpacks applies one value to every non-admin user and
	// returns the number of rows changed. includeManual=false skips rows an
	// admin set by hand; true overwrites them and clears their marker.
	BulkSetCanCreateModpacks(can bool, includeManual bool) (int64, error)

	// In-panel update feed: per-user acknowledged feed counts (platform,
	// gateway) so the navbar bell badge clears. Missing/legacy rows default to 0.
	GetUserUpdatesSeen(userID string) (version string, err error)
	SetUserUpdatesSeen(userID, version string) error

	// --- Setup wizard ---
	// CountUsers is declared above in the Users block; only CountAdmins is new.
	CountAdmins() (int, error)
	// CreateFirstAdmin atomically inserts the first admin via a guarded CTE
	// (guard: no admin exists yet). Returns ErrSetupAlreadyComplete when an
	// admin already exists, so the handler can map outcomes to HTTP status
	// codes without parsing strings.
	CreateFirstAdmin(username, passwordHash, totpSecret string) (*models.User, error)
	// CreateAdditionalAdmin unconditionally inserts another admin (break-glass
	// path). Returns ErrUsernameTaken on a username-unique violation.
	CreateAdditionalAdmin(username, passwordHash, totpSecret string) (*models.User, error)
}

// InactiveCandidate is the minimal slice of user data the auto-delete job
// needs to make a decision without fetching the whole row.
type InactiveCandidate struct {
	ID         string
	Username   string
	Email      string
	LastLogin  *time.Time
	HasHistory bool
}

// TicketFilter is the optional filter struct for ListTickets. Zero-value
// means "no filter applied" — every pointer/slice that's nil/empty is
// ignored. Limit is clamped to [1, 200] by the store; 0 falls back to 50.
type TicketFilter struct {
	// Visibility scope — exactly one of these is typically set per request.
	UserID         *string // user's own tickets
	AssignedUserID *string // tickets assigned to a supporter
	AssignedTeam   string  // tickets owned by a team (cross-team visibility scope)
	WatcherUserID  *string // tickets the user is CC'd on

	// Refinements layered on top of the scope.
	Status     []string // include only these statuses
	Priority   []string // include only these priorities
	CategoryID *int
	ServerUUID string
	Region     string

	// Pagination.
	Limit  int
	Offset int
}
