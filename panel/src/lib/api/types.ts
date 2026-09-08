import { API_URL as API_BASE } from './core';
import { handleUnauthorized } from './session';
import type { GatewayBandwidthOverview, BandwidthHistory } from '../bandwidth';

export interface AppModule {
    id: number;
    name: string;
    type: string;
    icon: string;
    url?: string;
    isEnabled: boolean;
    isSystem: boolean;
    position: number;
    accessRole: 'all' | 'admin';
}
export const setModuleAccessRole = (id: number, role: 'all' | 'admin') =>
    fetchAPI(`/modules/${id}/role`, { method: 'PATCH', body: JSON.stringify({ role }) });

export interface User {
    id: string;
    username: string;
    password?: string; // FIX: Allows setting passwords in the form
    email?: string;
    minecraftUsername?: string;
    isAdmin: boolean;
    is2FAEnabled?: boolean;
    createdAt?: string;
    lastUsernameChange?: string;
    // Region access. allRegionsAccess=true overrides the
    // explicit regions list — user sees all current and future regions.
    allRegionsAccess?: boolean;
    regions?: string[];
    // Lifecycle (surfaced for UI badges).
    emailVerifiedAt?: string;
    lastLoginAt?: string;
    deletionStatus?: string;
    // When status === 'pending_deletion', this is the date the
    // auto-delete job will execute. Surfaced in the admin UsersTab so admins
    // can see how urgent a rescue is.
    deletionScheduledAt?: string;
    deletionWarningSentAt?: string;
    // Roles + granular capability flags.
    role?: 'user' | 'support' | 'admin';
    canDeleteServers?: boolean;
    canChangeResources?: boolean;
    supportTeam?: string;
    // Per-user modpack authoring gate. Optional for forward-compat
    // with older API responses; the current backend always populates it.
    canCreateModpacks?: boolean;
    // True once an admin set canCreateModpacks by hand. Read-only: it is set as a
    // side effect of the per-user write, and it is what keeps that decision from
    // being flattened by the platform "Open authoring to users" switch.
    canCreateModpacksManual?: boolean;
}

export interface Node {
    id: number;
    name: string;
    address: string;
    token: string;
    status: string;
    lastSeenAt?: string;
    isLocal: boolean;
    tags?: string;
    region?: string;
    linkEnabled?: boolean;
    linkInstances?: number;
    cpusetCpus?: string;
    publicIp?: string;
    privateIps?: string[];
    serverCount?: number;
    // Placement (persisted)
    cpuOvercommitRatio?: number;
    ramOvercommitRatio?: number;
    totalCpu?: number;
    totalRamMb?: number;
    // Live (from heartbeat, set by infrastructure overview)
    cpuUsage?: number;
    ramFree?: number;
    ramTotal?: number;
    // Adoption state. configured=true once an admin set name/region/tags in the
    // panel (DB then wins over the heartbeat env). needsConfiguration=true when
    // the node has no region yet (booted with only a CLUSTER_SECRET).
    configured?: boolean;
    needsConfiguration?: boolean;
    // Optional, non-unique human label. Defaults to the node's hostname on
    // enroll; editable via configureNode independently of the unique `name`.
    displayName?: string;
}

// P0b-5 node admission + enroll/recovery tokens.
export interface AdmissionCIDR {
    id: string;
    cidr: string;
    label: string;
    createdAt: string;
}

export interface NodeEnrollToken {
    id: string;
    userId: string;
    label: string;
    createdAt: string;
    expiresAt?: string;
    consumedAt?: string;
}

export interface TabPermissions {
    console: boolean;
    files: boolean;
    config: boolean;
    setup: boolean;
    overview: boolean;
    power: boolean;
    // Newer than the rest: it comes from the resolved players.read, not from a
    // legacy invite blob, which has no such key. A legacy invite carrying
    // `power` maps to players.read on the backend, so those keep the tab.
    players: boolean;
    members: boolean;
    network: boolean;
    backups: boolean;
    inherit: boolean;
}

export interface ServerInvite {
    id: number;
    serverId: number;
    userId: string;
    username: string;
    email: string;
    permissions: TabPermissions;
    invitedBy: string;
    inviterName: string;
    createdAt: string;
}

export interface Server {
    id: number;
    uuid: string;
    name: string;
    nodeId: number;
    nodeName?: string;
    ownerId: string;
    ownerName?: string;
    owner?: string;
    node?: string;
    image?: string;
    port: number;
    memory: number;
    cpuLimit?: number;
    startCommand?: string;
    status: string;
    activeSubServer?: string;
    extraJvmFlags?: string;
    installerType?: string;
    minecraftVersion?: string;
    buildNumber?: string;
    diskLimit?: number;
    hostPort?: number;
    containerPort?: number;
    cpusetCpus?: string;
    cpuPinningMode?: 'shared' | 'auto' | 'manual';
    cpuset?: string;
    nodeAddress?: string;
    // Node reachability for the honest connectivity display (joined from nodes).
    nodeStatus?: string;
    nodeLastSeenAt?: string;
    serverType?: 'game' | 'proxy';
    proxyId?: number | null;
    createdAt?: string;
    role?: 'owner' | 'invited' | 'admin' | 'inherited' | 'demo';
    permissions?: TabPermissions;
    region?: string;
    // Read-only showcase server (admin-flagged). Non-owner viewers may only read;
    // the panel suppresses write affordances when this is set.
    isDemo?: boolean;
    // Set by Core when the server reads "installing" but nothing is working on
    // it, because the node holding the job is offline. The status stays
    // "installing" - it is still true, and the install resumes on its own when
    // the node returns. This is the explanation, not a different state.
    installStalled?: boolean;
    installStallReason?: string;
}

export interface SftpCredentials {
    host: string;
    port: number;
    username: string;
    path: string;
}

async function fetchAPI(endpoint: string, options: RequestInit = {}) {
    // No Authorization: the session cookie is sent automatically on these
    // same-origin requests.
    const headers = {
        'Content-Type': 'application/json',
        ...options.headers,
    };

    const res = await fetch(`${API_BASE}${endpoint}`, { ...options, headers });
    if (handleUnauthorized(res)) {
        return { success: false, error: 'Session expired', message: 'Session expired' };
    }
    // Some Go handlers send `http.Error(...)` which is text/plain. Trying to
    // parse that as JSON throws an unhelpful SyntaxError and callers lose the
    // server's actual message. Read the body once, parse it if it's JSON, and
    // otherwise wrap the text in a typed-shape envelope so callers can read
    // res.error / res.message uniformly.
    const text = await res.text();
    let parsed: any = null;
    try {
        parsed = text ? JSON.parse(text) : null;
    } catch {
        parsed = null;
    }
    if (parsed !== null) return parsed;
    if (!res.ok) {
        const msg = text.trim() || `Request failed with status ${res.status}`;
        return { success: false, error: msg, message: msg };
    }
    return {};
}

// --- PLATFORM HEALTH (admin Status page) ---
export type HealthStatus = 'up' | 'degraded' | 'down' | 'disabled';

export interface HealthItem {
    name: string;
    status: HealthStatus;
    detail?: string;
}

export interface HealthComponent {
    key: string;
    name: string;
    status: HealthStatus;
    detail?: string;
    reason?: string;
    items?: HealthItem[];
    /**
     * Machine-readable failure class, where the component has one. `status`
     * cannot carry this: two components can both be `down` for reasons that
     * need entirely different fixes, and a free-text `reason` is not something
     * the UI can branch on. Only the Redis component sets it today
     * (core/database/rediserror.go RedisFailure.Slug). Absent means the
     * component reports no classification and the card shows detail and reason
     * alone.
     */
    cause?: string;
}

export interface SystemHealth {
    overall: 'healthy' | 'degraded' | 'down';
    components: HealthComponent[];
    checkedAt: string;
}

export const getSystemHealth = (): Promise<{ success: boolean; health: SystemHealth }> =>
    fetchAPI('/admin/health');

// --- MODULES ---
export const getModules = () => fetchAPI('/modules');
export const createModule = (data: Partial<AppModule>) => fetchAPI('/modules', { method: 'POST', body: JSON.stringify(data) });
export const deleteModule = (id: number) => fetchAPI(`/modules/${id}`, { method: 'DELETE' });
export const toggleModule = (id: number, isEnabled: boolean) => fetchAPI(`/modules/${id}/toggle`, { method: 'PATCH', body: JSON.stringify({ isEnabled }) });
export const updateModulePosition = (id: number, position: number) => fetchAPI(`/modules/${id}/position`, { method: 'PATCH', body: JSON.stringify({ position }) });

// --- USERS ---
export const getUsers = () => fetchAPI('/users');
export const createUser = (data: Partial<User> & { allRegions?: boolean; regionsExplicit?: string[] }) => fetchAPI('/users', { method: 'POST', body: JSON.stringify(data) });
export const deleteUser = (id: string) => fetchAPI(`/users/${id}`, { method: 'DELETE' });
export const cancelUserDeletion = (id: string) => fetchAPI(`/admin/users/${id}/cancel-deletion`, { method: 'POST' });
export const setUserRole = (id: string, role: 'user' | 'support' | 'admin') =>
    fetchAPI(`/admin/users/${id}/role`, { method: 'PUT', body: JSON.stringify({ role }) });
/** Change an account's address. Core stores it UNVERIFIED and sends a
 *  verification when the policy requires one, because an admin typing an address
 *  has not shown that anyone reads it - and the reset link aims there. */
export const setUserEmail = (id: string, email: string) =>
    fetchAPI(`/admin/users/${id}/email`, { method: 'PATCH', body: JSON.stringify({ email }) });
export const setUserPermissions = (
    id: string,
    data: { canDeleteServers: boolean; canChangeResources: boolean; supportTeam?: string },
) => fetchAPI(`/admin/users/${id}/permissions`, { method: 'PUT', body: JSON.stringify(data) });

// Maintenance API
export interface MaintenanceState {
    active: boolean;
    title: string;
    message: string;
    expectedEnd: string;
    blockLevel: 'off' | 'banner_only' | 'block_writes' | 'block_all';
}
export const getMaintenance = () => fetchAPI('/maintenance');
export const saveMaintenance = (state: MaintenanceState) =>
    fetchAPI('/admin/maintenance', { method: 'PUT', body: JSON.stringify(state) });
export const resetUserPassword = (id: string, password: string) => fetchAPI(`/users/${id}/password`, { method: 'PUT', body: JSON.stringify({ password }) });
export const getUserRouteLimit = (id: string) => fetchAPI(`/users/${id}/route-limit`);
export const setUserRouteLimit = (id: string, data: { mode: string; maxRoutes: number }) => fetchAPI(`/users/${id}/route-limit`, { method: 'PUT', body: JSON.stringify(data) });

// --- NODES ---
export interface CpuCore { id: number; type: 'P' | 'E' | 'standard'; sibling: number; maxClockMHz: number; cacheGroup: number; l3KB: number; }
export interface NodeCpuTopology { logicalCount: number; physicalCount: number; hybrid: boolean; cores: CpuCore[]; scannedAt: number; }
// Returns { success, topology: NodeCpuTopology | null, load: { [coreId]: number } }.
export const getNodeCpu = (nodeId: number) => fetchAPI(`/nodes/${nodeId}/cpu`);
// How a node picks a storage path for NEW servers. "auto" takes the path with
// the most free space; "manual" walks `order` and takes the first usable path.
// Paths missing from `order` are tried last, so a disk added later needs no edit.
// Existing servers never move: their path is pinned at creation time.
export interface StoragePlacement { mode: 'auto' | 'manual'; order: string[]; }
// Returns { success, storage: StorageInfo[], placement: StoragePlacement }.
export const getNodeStorage = (nodeId: number) => fetchAPI(`/nodes/${nodeId}/storage`);
// Returns { success, nodeId, grpcTlsFingerprint, linkSecret, linkDiscoveryProof }.
// The secret-free BYON deploy bundle for an already-enrolled node.
export const getNodeDeployBundle = (nodeId: number) => fetchAPI(`/nodes/${nodeId}/deploy-bundle`);

/**
 * `scope` narrows the list to one kind of machine:
 *   'external' - the operator's own machines outside the swarm. Admin only;
 *                Core answers 403 to anyone else, so this is not a UI-side rule.
 *   'byon'     - machines the CALLER brought. Owner-scoped for everyone, admins
 *                included: it answers "your machines", and an admin's own are
 *                not the fleet's. The unscoped call is how an admin sees all.
 * Omitted returns everything the caller may see, which is what the node pickers
 * elsewhere in the panel want.
 */
export const getNodes = (scope?: 'external' | 'byon') =>
    fetchAPI(scope ? `/nodes?scope=${scope}` : '/nodes');
export const createNode = (data: Partial<Node>) => fetchAPI('/nodes', { method: 'POST', body: JSON.stringify(data) });
export const getNodeServers = (id: number) => fetchAPI(`/nodes/${id}/servers`);
export const forceDeleteNode = (id: number) => fetchAPI(`/nodes/${id}/force`, { method: 'DELETE' });
export const setNodeStoragePlacement = (id: number, placement: StoragePlacement) =>
    fetchAPI(`/nodes/${id}/storage-placement`, { method: 'PUT', body: JSON.stringify(placement) });
// The fleet-wide default a node uses when it has no policy of its own. Returns
// { success, placement, paths } - paths is every path ANY node reports, since
// no single node's list describes the fleet.
export const getFleetStoragePlacement = () => fetchAPI('/settings/storage-placement');
export const setFleetStoragePlacement = (placement: StoragePlacement) =>
    fetchAPI('/settings/storage-placement', { method: 'PUT', body: JSON.stringify(placement) });
// Adopt an auto-discovered node: persist name/region/tags to the DB. After this
// the heartbeat env no longer overwrites them.
export const configureNode = (id: number, data: { name?: string; region: string; tags?: string; displayName?: string }) =>
    fetchAPI(`/nodes/${id}/config`, { method: 'PATCH', body: JSON.stringify(data) });
// Set the node's container CPU pool (which host cores its containers may use).
// "" clears the restriction (all cores allowed).
export const updateNodeCpuset = (id: number, cpusetCpus: string) =>
    fetchAPI(`/nodes/${id}`, { method: 'PUT', body: JSON.stringify({ cpusetCpus }) });

// --- SERVERS ---
export const getServers = () => fetchAPI('/servers');
export const createServer = (data: any) => fetchAPI('/servers', { method: 'POST', body: JSON.stringify(data) });
export const deleteServer = (id: number) => fetchAPI(`/servers/${id}`, { method: 'DELETE' });
export const deleteSubServer = (id: number, subServerName: string) => fetchAPI(`/servers/${id}/sub-servers/${encodeURIComponent(subServerName)}`, { method: 'DELETE' });
export const serverPower = (id: number, action: string) => fetchAPI(`/servers/${id}/power`, { method: 'POST', body: JSON.stringify({ action }) });
// Seconds remaining on the post-install cooldown. Reads the same Redis TTL
// the backend's PowerAction handler enforces, so the UI shows the truth.
export const getInstallCooldown = (id: number): Promise<{ success: boolean; seconds: number }> =>
    fetchAPI(`/servers/${id}/install-cooldown`);
export const setupServer = (id: number, data: any) => fetchAPI(`/servers/${id}/setup`, { method: 'POST', body: JSON.stringify(data) });

/**
 * Change the Java image and the JVM flags WITHOUT reinstalling.
 *
 * setupServer re-runs the installer over the server directory, which is right
 * when the install is actually changing and destructive when it is not. This is
 * the non-destructive half: the node rebuilds the start command from what is
 * already on disk and recreates the container around it, leaving the server in
 * whatever run state it was in.
 */
export const updateServerRuntime = (
    id: number,
    data: { javaImage: string; extraJvmFlags: string },
): Promise<{ success: boolean; warning?: string; message?: string }> =>
    fetchAPI(`/servers/${id}/runtime`, { method: 'PATCH', body: JSON.stringify(data) });
export const switchSubServer = (id: number, subServerName: string) => fetchAPI(`/servers/${id}/switch`, { method: 'POST', body: JSON.stringify({ subServerName }) });
export const getSftpCredentials = (id: number) => fetchAPI(`/servers/${id}/sftp-credentials`);

// --- STORAGE MIGRATION ---
export interface StoragePathInfo {
    path: string;
    total_bytes: number;
    free_bytes: number;
    used_bytes: number;
    server_count: number;
}

export const getServerStoragePath = (id: number) => fetchAPI(`/servers/${id}/storage-path`);
export const migrateServerStorage = (id: number, targetPath: string) =>
    fetchAPI(`/servers/${id}/migrate-storage`, { method: 'POST', body: JSON.stringify({ targetPath }) });

// --- SERVER NAME / RESOURCES ---
export const updateServerName = (id: number, name: string) =>
    fetchAPI(`/servers/${id}/name`, { method: 'PATCH', body: JSON.stringify({ name }) });

// Metadata-only declare (no reinstall): persists installerType/minecraftVersion/
// buildNumber for an imported/uploaded server so the Content tab's loader/
// version auto-filtering starts working. See core DeclareServerLoaderMetadata.
export const declareServerLoaderMetadata = (
    id: number,
    installerType: string,
    minecraftVersion: string,
    buildNumber?: string,
) => fetchAPI(`/servers/${id}/loader-metadata`, {
    method: 'PATCH',
    body: JSON.stringify({ installerType, minecraftVersion, buildNumber: buildNumber || '' }),
});

export const updateServerResources = (
    id: number,
    ram: number,
    cpuLimit: number,
    diskLimit: number,
    ports?: { hostPort?: number; containerPort?: number },
    cpusetCpus?: string,
    // CPU pinning. Omit to leave pinning unchanged. For mode 'manual' the
    // cpuset is sent; for 'auto'/'shared' the backend ignores it.
    pinning?: { mode: 'shared' | 'auto' | 'manual'; cpuset?: string },
) => fetchAPI(`/servers/${id}/resources`, {
    method: 'PATCH',
    body: JSON.stringify({
        ram, cpuLimit, diskLimit,
        ...ports,
        ...(cpusetCpus !== undefined ? { cpusetCpus } : {}),
        ...(pinning !== undefined ? { cpuPinningMode: pinning.mode } : {}),
        ...(pinning !== undefined && pinning.cpuset !== undefined ? { cpuset: pinning.cpuset } : {}),
    }),
});

export const sendConsoleCommand = (id: number, command: string) =>
    fetchAPI(`/servers/${id}/console/command`, { method: 'POST', body: JSON.stringify({ command }) });

// --- PROXY LINKING ---
export const linkServerToProxy = (serverId: number, proxyId: number) =>
    fetchAPI(`/servers/${serverId}/proxy`, { method: 'PUT', body: JSON.stringify({ proxyId }) });
export const unlinkServerFromProxy = (serverId: number) =>
    fetchAPI(`/servers/${serverId}/proxy`, { method: 'DELETE' });

// --- BACKUPS ---
// 'connection' references a saved storage connection instead of carrying its
// own credentials, so rotating that connection updates every subsystem using it.
export type BackupProvider = 'local' | 's3' | 'node-local' | 'shared' | 'core-storage' | 'connection';

// Config blob for the 'connection' provider. Holds no credentials on purpose.
export interface BackupConnectionConfig {
    connectionId: number;
    /** Key prefix inside the bucket. Defaults to "server-backups" server-side. */
    prefix?: string;
}

export interface BackupStorage {
    id: number;
    name: string;
    provider: BackupProvider;
    config: Record<string, unknown>;
    isDefault: boolean;
    createdAt?: string;
    /**
     * True when an s3 secret is stored. The secret itself is never returned by
     * the API (redacted server-side), so the edit form shows the field empty
     * and leaving it blank keeps the stored secret.
     */
    secretSet?: boolean;
}

// Global backup-mode + quota config (persisted server-side under
// backup.mode / backup.quota_per_server_gb / backup.share_quota_with_server).
// The mode picks which provider the panel exposes as creatable; per-instance
// credentials still live in the BackupStorage rows above.
export interface BackupConfig {
    mode: 'shared' | 's3' | 'node-local';
    // null = no cap, 0 = none (node-local backups not allowed), n = the cap.
    quotaPerServerGb: number | null;
    // What the PLATFORM holds for a server owner who bought nothing. Applies in
    // every mode, unlike quotaPerServerGb above, which bounds one server's
    // folder on one node's disk.
    defaultUserQuotaGb: number | null;
    shareQuotaWithServer: boolean;
}

export const getBackupConfig = (): Promise<{ success: boolean; settings?: BackupConfig }> =>
    fetchAPI('/settings/backup');
export const saveBackupConfig = (cfg: BackupConfig): Promise<{ success: boolean; message?: string }> =>
    fetchAPI('/settings/backup', { method: 'POST', body: JSON.stringify(cfg) });

// Per-server backup-folder usage (bytes on disk, archive count). Used by
// the Overview tab to render a separate or combined storage bar when the
// global backup.mode is "node-local". `degraded` is true when the Node
// couldn't be reached — the UI uses that to suppress the row instead of
// rendering misleading zeros.
export interface BackupUsage {
    success: boolean;
    usedBytes: number;
    count: number;
    degraded?: boolean;
}
export const getBackupUsage = (serverId: number): Promise<BackupUsage> =>
    fetchAPI(`/servers/${serverId}/backup-usage`);
export interface BackupJob {
    id: number;
    serverId: number;
    subServer?: string | null;
    name: string;
    schedule: string;             // "manual" | "every Nh" | "every Nd"
    includePatterns: string[];
    excludePatterns: string[];
    retentionCount: number;
    storageId?: number | null;
    enabled: boolean;
    lastRunAt?: string | null;
    nextRunAt?: string | null;
    createdAt?: string;
}
export interface BackupRun {
    id: number;
    jobId: number;
    startedAt: string;
    completedAt?: string | null;
    status: 'running' | 'success' | 'failed';
    sizeBytes: number;
    storageKey: string;
    errorMessage: string;
}

export const listBackupStorages = (): Promise<{ success: boolean; storages?: BackupStorage[] }> =>
    fetchAPI('/backup-storages');
// message carries the server's reason on a rejected write (409 for a duplicate
// name, 404 for a storage that no longer exists). Declared so the UI can show
// it instead of a bare "Save failed."
export const createBackupStorage = (s: Partial<BackupStorage>): Promise<{ success: boolean; id?: number; message?: string }> =>
    fetchAPI('/backup-storages', { method: 'POST', body: JSON.stringify(s) });
export const updateBackupStorage = (id: number, s: Partial<BackupStorage>): Promise<{ success: boolean; message?: string }> =>
    fetchAPI(`/backup-storages/${id}`, { method: 'PATCH', body: JSON.stringify(s) });
export const deleteBackupStorage = (id: number): Promise<{ success: boolean; message?: string }> =>
    fetchAPI(`/backup-storages/${id}`, { method: 'DELETE' });
// The signal is how the shared test lifecycle (lib/connectionTest.ts) stops
// waiting. `stage` says whether the endpoint answered at all - a dead endpoint
// and a rejected key are different problems and no longer share a message.
export const testBackupStorage = (id: number, signal?: AbortSignal): Promise<{ success: boolean; stage?: string; message?: string; warning?: string }> =>
    fetchAPI(`/backup-storages/${id}/test`, { method: 'POST', signal });

// --- STORAGE CONNECTIONS ---
// Named, reusable storage backends (currently s3) that any feature can
// reference instead of re-entering credentials. The secret access key lives
// encrypted server-side in its own column and is never returned by the API.

export interface StorageConnectionConfig {
    endpoint?: string;
    region?: string;
    bucket?: string;
    forcePathStyle?: boolean;
    prefix?: string;
}

export interface StorageConnection {
    id: number;
    name: string;
    provider: 's3';
    config: StorageConnectionConfig;
    accessKey: string;
    createdAt?: string;
    updatedAt?: string;
    /**
     * True when a secret is stored. The secret itself is never returned by the
     * API, so the edit form shows the field empty; leaving it blank keeps the
     * stored secret.
     */
    secretSet?: boolean;
}

// Write payload: the secret is sent separately (write-only) and omitted on an
// edit that keeps the existing one.
export interface StorageConnectionInput {
    name: string;
    provider: 's3';
    config: StorageConnectionConfig;
    accessKey: string;
    secretAccessKey?: string;
}

export const listStorageConnections = (): Promise<{ success: boolean; connections?: StorageConnection[] }> =>
    fetchAPI('/storage-connections');
// message: see createBackupStorage - same 409/404 reasons apply here.
export const createStorageConnection = (c: StorageConnectionInput): Promise<{ success: boolean; id?: number; message?: string }> =>
    fetchAPI('/storage-connections', { method: 'POST', body: JSON.stringify(c) });
export const updateStorageConnection = (id: number, c: StorageConnectionInput): Promise<{ success: boolean; message?: string }> =>
    fetchAPI(`/storage-connections/${id}`, { method: 'PATCH', body: JSON.stringify(c) });
export const deleteStorageConnection = (id: number): Promise<{ success: boolean; message?: string }> =>
    fetchAPI(`/storage-connections/${id}`, { method: 'DELETE' });
export const testStorageConnection = (id: number, signal?: AbortSignal): Promise<{ success: boolean; ok?: boolean; stage?: string; message?: string }> =>
    fetchAPI(`/storage-connections/${id}/test`, { method: 'POST', signal });

// Test a connection that has NOT been saved, from inside the dialog.
//
// `id` names the saved connection whose stored secret may be borrowed when the
// form leaves the secret field blank. The server only allows that when the
// endpoint, bucket and access key are unchanged - otherwise it would be a way to
// point somebody else's credential at a host of your choosing. Pass 0 for a new
// connection, which then has to carry its own secret.
export const testDraftStorageConnection = (
    c: StorageConnectionInput & { id: number },
    signal?: AbortSignal,
): Promise<{ success: boolean; ok?: boolean; stage?: string; message?: string }> =>
    fetchAPI('/storage-connections/test', { method: 'POST', body: JSON.stringify(c), signal });

export const listBackupJobs = (serverId: number): Promise<{ success: boolean; jobs?: BackupJob[] }> =>
    fetchAPI(`/servers/${serverId}/backup-jobs`);
export const createBackupJob = (serverId: number, j: Partial<BackupJob>): Promise<{ success: boolean; id?: number }> =>
    fetchAPI(`/servers/${serverId}/backup-jobs`, { method: 'POST', body: JSON.stringify(j) });
export const updateBackupJob = (jobId: number, j: Partial<BackupJob>): Promise<{ success: boolean }> =>
    fetchAPI(`/backup-jobs/${jobId}`, { method: 'PATCH', body: JSON.stringify(j) });
export const deleteBackupJob = (jobId: number): Promise<{ success: boolean }> =>
    fetchAPI(`/backup-jobs/${jobId}`, { method: 'DELETE' });
// message is declared because Core sends one on every refusal here (quota
// reached, no storage configured, node unreachable) and a type without it
// makes the reason unreachable at the call site - which is how "Run Now"
// ended up silently doing nothing.
export const triggerBackupJob = (jobId: number): Promise<{ success: boolean; runId?: number; message?: string }> =>
    fetchAPI(`/backup-jobs/${jobId}/trigger`, { method: 'POST' });
export const listBackupRuns = (jobId: number): Promise<{ success: boolean; runs?: BackupRun[] }> =>
    fetchAPI(`/backup-jobs/${jobId}/runs`);
export const deleteBackupRun = (runId: number): Promise<{ success: boolean }> =>
    fetchAPI(`/backup-runs/${runId}`, { method: 'DELETE' });
export const restoreBackupRun = (runId: number): Promise<{ success: boolean; message?: string; restoreId?: number }> =>
    fetchAPI(`/backup-runs/${runId}/restore`, { method: 'POST' });

export interface BackupRestore {
    id: number;
    runId: number;
    serverId: number;
    requestedBy?: string | null;
    requestedAt: string;
    completedAt?: string | null;
    status: 'queued' | 'running' | 'success' | 'failed';
    errorMessage: string;
    /** Set at response time, not stored: still queued while the node that would
     *  run it is gone. The status stays "queued" because the job sits on the
     *  node's durable stream and really does resume - so this explains the wait
     *  rather than declaring a failure. */
    stalled?: boolean;
    stallReason?: string;
}
export const listBackupRestores = (serverId: number): Promise<{ success: boolean; restores?: BackupRestore[] }> =>
    fetchAPI(`/servers/${serverId}/backup-restores`);
export const backupDownloadUrl = (runId: number) => {
    // A plain anchor cannot send an Authorization header, which is why this
    // used to carry ?token=. It no longer needs to: the link is same-origin, so
    // the browser attaches the session cookie by itself.
    return `${API_BASE}/backup-runs/${runId}/download`;
};

export interface ProxyEndpoint {
    serverId: number;
    serverName: string;
    proxyId: number;
    proxyUuid: string;
    ip: string;
    hostname: string;
}
export const getProxyEndpoint = (serverId: number): Promise<{ success: boolean; endpoints?: ProxyEndpoint[] }> =>
    fetchAPI(`/servers/${serverId}/proxy-endpoint`);


// --- LIBRARY ---
export const getLibraryFiles = (path?: string) => fetchAPI(`/library${path ? `?path=${encodeURIComponent(path)}` : ''}`);
export const deleteLibraryPath = (path: string) => fetchAPI('/library/delete', { method: 'POST', body: JSON.stringify({ path }) });
export const createLibraryDir = (path: string) => fetchAPI('/library/mkdir', { method: 'POST', body: JSON.stringify({ path }) });
export const toggleLibraryPath = (path: string, enabled: boolean) =>
    fetchAPI('/library/toggle', { method: 'POST', body: JSON.stringify({ path, enabled }) });
// Library storage backend (path/s3) config now lives at Settings -> Core
// Storage (@/lib/api/coreStorage) - the old /settings/library CRUD + test
// endpoints were dead (Library reads its provider from the shared Core file
// storage config) and have been removed on both sides.

// --- FILE MANAGER SETTINGS ---
// On the platform limit convention: null is no limit, 0 refuses the transfer,
// n is the ceiling in bytes. They used to be plain numbers whose enforcement
// guarded on `> 0`, so a stored 0 meant "use the default" and neither extreme
// was reachable from the screen.
export interface FileManagerSettings {
    adminUploadLimit: number | null;
    adminDownloadLimit: number | null;
    userUploadLimit: number | null;
    userDownloadLimit: number | null;
}
export const getFileManagerSettings = () => fetchAPI('/settings/filemanager');
export const saveFileManagerSettings = (data: FileManagerSettings) => fetchAPI('/settings/filemanager', { method: 'POST', body: JSON.stringify(data) });
export const getUserLimits = () => fetchAPI('/settings/filemanager/limits');

// --- STATS ---
export interface ServerStats {
    ts: number;
    cpu: number;
    cpuLimit: number;
    memUsed: number;
    memLimit: number;
    // Post-GC live JVM heap size in MB. Populated by the log-shipper's
    // GC log parser; absent on Java 8 (which can't emit -Xlog), or for
    // the first few seconds of a fresh boot before the JVM has GC'd.
    // When present this is the "real" memory usage that fluctuates with
    // GC cycles; memUsed is the container-level metric that always
    // reads near Xmx because the heap is pre-committed.
    javaHeapUsed?: number;
    players: number;
    maxPlayers: number;
    motd: string;
}

export interface DiskUsage {
    total: number;
    limit: number;
    subServers: Record<string, number>;
    warning?: '' | '80' | '90' | 'full';
    // Whether `limit` is actually ENFORCED, or only recorded. Project quotas
    // need xfs or ext4; on NFS, CIFS or a Docker Desktop bind mount from a
    // Windows host the number is stored and nothing holds a server to it.
    //
    // Optional on purpose: a node that predates the field sends nothing, and
    // `undefined` must not be read as "not enforced" or every server would carry
    // the warning until the whole fleet is updated. Only an explicit false means
    // the node said so.
    enforceable?: boolean;
}

export const getStatsHistory = (id: number, range?: string) =>
    fetchAPI(`/servers/${id}/stats/history${range ? `?range=${range}` : ''}`);

export const getDiskUsage = (id: number) => fetchAPI(`/servers/${id}/stats/disk`);

// --- SERVER SETTINGS ---
export interface ServerLimitSettings {
    // null = no cap, 0 = none may be created, n = the cap.
    maxSubServers: number | null;
}
export const getServerSettings = () => fetchAPI('/settings/servers');
export const saveServerSettings = (data: ServerLimitSettings) => fetchAPI('/settings/servers', { method: 'POST', body: JSON.stringify(data) });

// --- FEATURE SETTINGS ---
export interface FeatureSettings {
    proxyEnabled: boolean;
}
export const getFeatureSettings = () => fetchAPI('/settings/features');
export const saveFeatureSettings = (data: FeatureSettings) => fetchAPI('/settings/features', { method: 'POST', body: JSON.stringify(data) });

// --- GATEWAY ---
export interface GatewayLink {
    ID: number;
    name: string;
    token: string;
    enabled: boolean;
    is_system: boolean;
    node_id?: string;
    online: boolean;
}

export interface EdgeStats {
    cpu: number;
    ram_used: number;
    ram_total: number;
    ram_pct: number;
    rx_speed: number;
    tx_speed: number;
    active_tokens: number;
    active_mc_streams: number;
    xdp_enabled: boolean;
    xdp_passed: number;
    xdp_dropped_blocked: number;
    xdp_dropped_ratelimit: number;
    xdp_blocked_ips: number;
}

export interface GatewayEdge {
    edge_id: string;
    name: string;
    ip: string;
    private_ip: string;
    service_port: string;
    splice_port: string;
    status: string;
    region?: string;
    // splice_version = the RUNNING splice sidecar on this edge; splice_version_latest
    // = the LATEST available splice (baked into the edge's rolling :latest image).
    // Compared per region to flag a pending, deliberately-scheduled splice bump.
    // Empty when a pre-versioning edge/splice is deployed.
    splice_version?: string;
    splice_version_latest?: string;
    // The IMAGE the splice container runs, and the image SPLICE_IMAGE resolves
    // to. The version pair above cannot answer "is the running splice the
    // pinned one": a tag deleted from the registry frees the name for a later
    // build, so two different images report the same version. Empty means the
    // edge could not tell - never read two empties as agreement.
    splice_image_running?: string;
    splice_image_available?: string;
    stats?: EdgeStats;
}

export interface GatewayRoute {
    // domain is the unique identity — routes live in Redis as
    // route:{domain} and have no integer ID. Use it as the React key
    // and as the URL segment when deleting.
    domain: string;
    target_ip: string;
    target_port: number;
    link_id?: number;
    link?: GatewayLink;
    server_uuid?: string;
    server_id?: number;
    owner_id?: string;
    owner_name?: string;
    server_name?: string;
    link_name?: string;
}

export interface GatewayLog {
    id: number;
    timestamp: string;
    level: string;
    source: string;
    message: string;
}

export interface ServiceError {
    timestamp: string;
    level: string;
    source: string;
    message: string;
}

/**
 * Address caps per scope. Each follows the platform convention:
 * null = no limit, 0 = none, n = the cap. See LimitField.
 *
 * These were plain numbers where -1 meant unlimited, which could not express
 * "none" - while the help text under the very same inputs already promised
 * "0 = zero routes allowed". The UI was right and the wire format could not
 * carry it.
 */
export interface GatewayLimits {
    global: number | null;
    userDefault: number | null;
    portMcEnabled: boolean;
}

export type HosterValidation = 'letters' | 'alphanumeric' | 'dns';

export interface HosterDomain {
    domain: string;
    validation: HosterValidation;
}

export interface GatewaySettings {
    limits: GatewayLimits;
    hosterDomains: HosterDomain[];
    customDomainsEnabled: boolean;
    cnameTarget: string;
    // Reserved leftmost labels users may not register as a route (e.g. admin, dylaris).
    blockedRoutePrefixes: string[];
}

export interface GatewayRouteOptions {
    success: boolean;
    hosterDomains: HosterDomain[];
    customDomainsEnabled: boolean;
    cnameTarget: string;
}

// Gateway Admin API (read-only for links/edges — they auto-register via Redis)
export const getGatewayRoutes = () => fetchAPI('/gateway/routes');
export const deleteGatewayRoute = (domain: string) => fetchAPI(`/gateway/routes/${encodeURIComponent(domain)}`, { method: 'DELETE' });

// Bulk route operations (admin)
export interface RouteSuffix {
    suffix: string;
    count: number;
    depth: number; // 1 = apex (one dot), 2 = parent (two dots)
}
export const getRouteSuffixes = (): Promise<{ success: boolean; suffixes: RouteSuffix[] }> =>
    fetchAPI('/gateway/routes/suffixes');
export const bulkDeleteRoutesBySuffix = (suffix: string): Promise<{ success: boolean; deleted: number; failed: number; suffix: string; message?: string }> =>
    fetchAPI('/gateway/routes/bulk-delete', { method: 'POST', body: JSON.stringify({ suffix }) });
export const triggerGatewaySync = () => fetchAPI('/gateway/sync', { method: 'POST' });

export const getGatewaySettings = () => fetchAPI('/settings/gateway');
export const saveGatewaySettings = (data: GatewaySettings) => fetchAPI('/settings/gateway', { method: 'POST', body: JSON.stringify(data) });

// --- PLACEMENT / SCHEDULING ---
export interface PlacementSettings {
    cpuOvercommitDefault: number;
    ramOvercommitDefault: number;
    diskBufferGb: number;
    rebalanceEnabled: boolean;
    rebalanceThreshold: number;
    portMode: 'sequential' | 'random';
    containerPort: number;
    // Per-container process/thread cap (cgroup pids). 0 = unlimited.
    pidsLimit: number;
    // Per-container blkio relative weight (10–1000). 0 = off. Scheduler-dependent.
    ioWeight: number;
    // What happens when a placement would eat into the disk buffer: "soft"
    // places it anyway and reports it, "hard" refuses. Governs ADMISSION only -
    // it never stops an already-running server from writing.
    diskEnforcement: 'soft' | 'hard';
    // PROJECTED fill levels (written + still promised, over total) at which a
    // storage path is flagged in the panel.
    diskWarnPercent: number;
    diskCriticalPercent: number;
}
export interface NodeCandidate {
    nodeId: number;
    nodeName: string;
    available: boolean;
    reason: string;
    score: number;
    allocRamMb: number;
    allocCpu: number;
    totalRamMb: number;
    totalCpu: number;
    overcommitRam: number;
    overcommitCpu: number;
    serverCount: number;
}
export interface PickNodeResponse {
    success: boolean;
    picked?: NodeCandidate;
    candidates: NodeCandidate[];
    reason: string;
}
export const getPlacementSettings = () => fetchAPI('/settings/placement');
export const savePlacementSettings = (data: PlacementSettings) =>
    fetchAPI('/settings/placement', { method: 'POST', body: JSON.stringify(data) });
export const getAvailableTags = (region?: string): Promise<{ success: boolean; tags: string[] }> =>
    fetchAPI(`/placement/tags${region ? `?region=${encodeURIComponent(region)}` : ''}`);
export const getAvailableRegions = (): Promise<{ success: boolean; regions: string[] }> =>
    fetchAPI('/placement/regions');
export const pickNode = (data: { region?: string; tags?: string[]; tag?: string; nodeId?: number; ramMb: number; cpuCores: number; diskGb: number }): Promise<PickNodeResponse> =>
    fetchAPI('/placement/pick', { method: 'POST', body: JSON.stringify(data) });
export const setNodePlacement = (nodeId: number, data: { cpuOvercommitRatio: number; ramOvercommitRatio: number }) =>
    fetchAPI(`/nodes/${nodeId}/placement`, { method: 'PUT', body: JSON.stringify(data) });
export const setServerAutoMove = (serverId: number, enabled: boolean) =>
    fetchAPI(`/servers/${serverId}/automove`, { method: 'PATCH', body: JSON.stringify({ enabled }) });

// Manual node-to-node migration (admin). Async — the Core enqueues onto the
// orchestrator and returns 202; progress is polled via getMigrationStatus.
export const moveServer = (serverId: number, targetNodeId: number) =>
    fetchAPI(`/admin/servers/${serverId}/move`, { method: 'POST', body: JSON.stringify({ targetNodeId }) });

// Tenant-facing transfer (BYON). Same orchestrator + status as moveServer, but
// the caller only needs to own the server and be allowed to place on the target
// node. Async — returns 202; progress is polled via getMigrationStatus.
export const transferServer = (serverId: number, targetNodeId: number) =>
    fetchAPI(`/servers/${serverId}/transfer`, { method: 'POST', body: JSON.stringify({ targetNodeId }) });

// Mark/unmark a server as a public read-only demo (admin). Non-owner users with
// no server of their own then see it read-only in their sidebar.
export const setServerDemo = (serverId: number, enabled: boolean) =>
    fetchAPI(`/admin/servers/${serverId}/demo`, { method: 'PATCH', body: JSON.stringify({ enabled }) });

// Orchestrator progress record. `phase` is "none" when no migration is active.
// Terminal phases: done, failed, failed_post_cutover, aborted_players, none.
export interface MigrationStatus {
    phase: string;
    error?: string;
    sourceNodeID?: number;
    targetNodeID?: number;
    reason?: string;
    startedAt?: number;
    updatedAt?: number;
    // True only while the migration is still pre-cutover and in-flight, so an
    // admin cancel would actually take effect (the panel hides the cancel button
    // otherwise). Computed server-side in GetMigrationStatus.
    cancellable?: boolean;
}
export const getMigrationStatus = (serverId: number): Promise<{ success: boolean; status?: MigrationStatus; message?: string }> =>
    fetchAPI(`/servers/${serverId}/migration-status`);

// Cancel an in-flight migration (admin). Rolls the server back to its current
// node - only honored pre-cutover; returns 409 once the move has cut over.
export const cancelMigration = (serverId: number) =>
    fetchAPI(`/admin/servers/${serverId}/migration/cancel`, { method: 'POST' });

// Per-server edge transitional-MOTD config (what players see via the gateway
// edge while the server is down/starting/migrating).
export type EdgeMotdMode = 'auto' | 'custom' | 'off';
export const getEdgeMotd = (serverId: number): Promise<{ success: boolean; mode?: EdgeMotdMode; customText?: string; message?: string }> =>
    fetchAPI(`/servers/${serverId}/edge-motd`);
export const setEdgeMotd = (serverId: number, mode: EdgeMotdMode, customText: string) =>
    fetchAPI(`/servers/${serverId}/edge-motd`, { method: 'PATCH', body: JSON.stringify({ mode, customText }) });

// Infrastructure
export const getInfrastructureOverview = () => fetchAPI('/infrastructure/overview');
export const getRoutingMigrationStatus = () => fetchAPI('/infrastructure/routing-migration');

// Gateway bandwidth dashboard (F2). Types + pure display helpers live in ../bandwidth.
export const getGatewayBandwidthOverview = (): Promise<GatewayBandwidthOverview> =>
    fetchAPI('/gateway-bandwidth/overview');

// Every series at once: one per component and one per host. One request rather
// than one per card - the screen draws a sparkline for each component plus a
// chart for whatever is selected, and that is seven round trips for data that
// comes from a single table scan.
export const getGatewayBandwidthHistory = (
    range: string,
): Promise<BandwidthHistory> =>
    fetchAPI(`/gateway-bandwidth/history?range=${encodeURIComponent(range)}`);

// F3 warp rebalancer: mode + recent decision feed, surfaced in the Bandwidth panel.
export interface WarpMove {
    pubkey: string;
    from: string;
    to: string;
    txBps: number;
}
export interface WarpDecision {
    ts: number;
    mode: string;
    applied: boolean;
    moves: WarpMove[];
    note?: string;
}
export interface RebalanceView {
    mode: 'off' | 'dry-run' | 'armed' | string;
    decisions: WarpDecision[];
}
export const getGatewayRebalance = (): Promise<RebalanceView> =>
    fetchAPI('/gateway-bandwidth/rebalance');

// setWarpRebalanceMode writes the F3 rebalancer mode. There is no generic
// per-key settings writer in this codebase to reuse here - every /settings/*
// endpoint POSTs a fixed domain struct (features, gateway, placement,
// routing-mode, warp-firewall, ...). This POSTs to the same resource the GET
// reads, mirroring the GET-reads/POST-writes-same-path convention already
// used for routing-mode and warp-firewall. Core's handler (settings.write)
// returns `{ success: true, mode }` on success and `{ success: false, message }`
// on a rejected mode / permission / save failure - callers must check
// `res.success` themselves, fetchAPI does not throw on a non-2xx response.
export const setWarpRebalanceMode = (
    mode: 'off' | 'dry-run' | 'armed',
): Promise<{ success?: boolean; message?: string; mode?: string }> =>
    fetchAPI('/gateway-bandwidth/rebalance', { method: 'POST', body: JSON.stringify({ mode }) });

// Routing Mode
export type RoutingMode = 'ip_port' | 'both' | 'gateway';
export type FileAccessMode = 'sftp' | 'both' | 'beam';

// isGatewayRouting MUST stay byte-equivalent to Core's AppState.gatewayEnabled
// (core/handlers/gateway_gate.go), which is what gates every gateway write.
// Any UI that shows or hides gateway surfaces has to ask this, not a separate
// flag, or the panel and Core disagree about whether the gateway is on.
export const isGatewayRouting = (mode: RoutingMode): boolean =>
    mode === 'gateway' || mode === 'both';

// feature_byon_enabled is the operator's INTENT; this is whether BYON can
// actually work. A tenant node forces gateway routing and beam on its own side
// (NODE_EXTERNAL), so on ip_port there is nothing for it to join. Reading the
// raw flag let an operator with no gateway - which is every self-host build,
// since the gateway is not part of the open-core stack - switch on a whole
// tenant surface: enrolment screens for machines that can never connect, and
// the Usage/Billing/Plans settings behind them.
//
// Same shape as autoMove, whose flag is documented as one "the panel ANDs with
// the live routing mode".
export const isByonUsable = (byonFlag: boolean, mode: RoutingMode): boolean =>
    byonFlag && isGatewayRouting(mode);
export const getRoutingMode = () => fetchAPI('/settings/routing-mode');
export const saveRoutingMode = (data: { mode: RoutingMode; fileMode: FileAccessMode }) =>
    fetchAPI('/settings/routing-mode', { method: 'POST', body: JSON.stringify(data) });

// --- Warp (external/home node WireGuard bridge, multi-hub) ---
// A region owns one WG identity (subnet + key); its leaders are interchangeable
// endpoints. Clients fail over between a region's leaders without changing IP.
export interface WarpLeaderView {
    leaderId: string;
    endpoint: string;
    enabled: boolean;
    alive: boolean;
}
export interface WarpRegionView {
    region: string;
    subnet: string;
    enabled: boolean;
    peerCount: number;
    leaders: WarpLeaderView[] | null;
}
export interface MintWarpKeyInput {
    name: string;
    policy: 'fixed' | 'general';
    max_conns: number;
    on_new_conn: 'kill_old' | 'block';
    region?: string; // "" = auto-assign at enroll (least-loaded live region)
}
export const getWarpRegions = () => fetchAPI('/warp/regions');
export const upsertWarpRegion = (data: { region: string; subnet: string; enabled: boolean }) =>
    fetchAPI('/warp/regions', { method: 'POST', body: JSON.stringify(data) });
export const deleteWarpRegion = (region: string) =>
    fetchAPI(`/warp/regions/${encodeURIComponent(region)}`, { method: 'DELETE' });
export const upsertWarpLeader = (data: { leaderId: string; region: string; endpoint: string; enabled: boolean }) =>
    fetchAPI('/warp/leaders', { method: 'POST', body: JSON.stringify(data) });
export const deleteWarpLeader = (leaderId: string) =>
    fetchAPI(`/warp/leaders/${encodeURIComponent(leaderId)}`, { method: 'DELETE' });
export const mintWarpKey = (data: MintWarpKeyInput) =>
    fetchAPI('/admin/warp/keys', { method: 'POST', body: JSON.stringify(data) });

// One enrolled client of a key. A key with no peers was minted but never used.
export interface WarpKeyPeer {
    pubkey: string;
    wg_ip: string;
    region: string;
    assigned_leader: string;
    created_at: string;
}
// The enrollment-key inventory. The key itself is never returned - only its
// hash is stored, so it exists in readable form exactly once, at mint time.
export interface WarpKeyView {
    id: number;
    name: string;
    policy: 'fixed' | 'general';
    max_conns: number;
    on_new_conn: 'kill_old' | 'block';
    region: string;
    node_id: string;
    fixed_wg_ip: string;
    revoked: boolean;
    created_at: string;
    peers: WarpKeyPeer[];
}
export const listWarpKeys = (): Promise<{ success: boolean; keys: WarpKeyView[] }> =>
    fetchAPI('/admin/warp/keys');
// Revoking also disconnects live peers: an established WireGuard tunnel has no
// memory of the key that created it and would otherwise keep forwarding.
export const revokeWarpKey = (id: number): Promise<{ success: boolean; disconnected?: number; message?: string }> =>
    fetchAPI(`/admin/warp/keys/${id}`, { method: 'DELETE' });
// Removes the row entirely. Disconnects peers first, server-side: warp_peers
// cascades, so deleting the row before pushing the peers out would leave a live
// WireGuard peer with no record that could ever remove it.
export const deleteWarpKey = (id: number): Promise<{ success: boolean; disconnected?: number; message?: string }> =>
    fetchAPI(`/admin/warp/keys/${id}/purge`, { method: 'DELETE' });

// Warp overlay segmentation: the admin-configurable spoke destination-port
// allowlist the region leaders enforce (comma-separated TCP ports).
export interface WarpFirewallSettings {
    allowedPorts: string;
    /**
     * DC overlay CIDR(s) a warp client routes through the tunnel - where Redis
     * and Core gRPC live. Core cannot infer this (it never sees the Docker
     * overlay), so it is stored once here and handed out in deploy snippets.
     * Comma-separated; "" means unset and the snippet shows a placeholder.
     */
    tunnelSubnets: string;
}
// Detected, not stored: what Core works out the overlay CIDR to be, so a
// self-hoster never has to look it up. Candidates are most-confident first.
export interface SuggestedTunnelSubnets {
    suggested: string;
    candidates: string[];
    source: string;
}
export const getWarpFirewallSettings = (): Promise<{ success: boolean; settings: WarpFirewallSettings; suggestedTunnelSubnets?: SuggestedTunnelSubnets }> =>
    fetchAPI('/settings/warp-firewall');
export const saveWarpFirewallSettings = (
    data: WarpFirewallSettings,
): Promise<{ success: boolean; settings?: WarpFirewallSettings; error?: string; message?: string }> =>
    fetchAPI('/settings/warp-firewall', { method: 'POST', body: JSON.stringify(data) });

// Gateway User API (per-server routes)
export const getServerRoutes = (serverId: number) => fetchAPI(`/servers/${serverId}/routes`);
export interface CreateRouteRequest {
    targetPort: number;
    // Either a hoster-picker pair, a custom-domain string, or a raw domain (legacy/admin).
    subdomain?: string;
    hosterDomain?: string;
    customDomain?: string;
    domain?: string;
}
export const createServerRoute = (serverId: number, data: CreateRouteRequest) =>
    fetchAPI(`/servers/${serverId}/routes`, { method: 'POST', body: JSON.stringify(data) });
// Backend identifies routes by full domain (URL: /servers/{id}/routes/{domain:.+}).
export const deleteServerRoute = (serverId: number, domain: string) =>
    fetchAPI(`/servers/${serverId}/routes/${encodeURIComponent(domain)}`, { method: 'DELETE' });
export const getGatewayRouteOptions = (): Promise<GatewayRouteOptions> => fetchAPI('/gateway/route-options');

// Route-only ("via Link") routes: a protected address pointed at a server the
// user runs on their OWN machine, reached through their own outbound Link tunnel
// (no managed node, no exposed origin). Owner-scoped.
export interface LinkRoute {
    domain: string;
    target_ip: string;
    target_port: number;
    /**
     * Which link kit this route runs through. Resolved by Core, which is the
     * only side that can: the route itself stores the link's derived tunnel
     * TOKEN - a credential, and no longer part of this response - while the
     * panel knows kits by their link id.
     */
    link_id?: string;
    core_owned?: boolean;
    owner_id?: string;
}
export interface CreateLinkRouteRequest extends CreateRouteRequest {
    linkId: string;    // the Link kit's id (warp key node_id)
    targetHost: string; // the LOCAL target the Link dials (LAN / loopback allowed)
}
export const getLinkRoutes = (): Promise<LinkRoute[]> => fetchAPI('/gateway/link-routes');
export const createLinkRoute = (data: CreateLinkRouteRequest) =>
    fetchAPI('/gateway/link-routes', { method: 'POST', body: JSON.stringify(data) });
export const deleteLinkRoute = (domain: string) =>
    fetchAPI(`/gateway/link-routes/${encodeURIComponent(domain)}`, { method: 'DELETE' });

// Link kits: a warp key + auto link identity bound to the account. The customer
// runs warp + link with the kit to expose a LOCAL server through the gateway.
// Mint returns the secrets ONCE; the list is metadata-only.
export interface LinkKit {
    id: number;
    name: string;
    link_id: string;
    created_at: string;
}
export interface MintedLinkKit {
    success: boolean;
    warp_key: string;
    link_id: string;
    note: string;
}
export const listLinkKits = (): Promise<{ success: boolean; kits: LinkKit[]; used?: number; limit?: number }> =>
    fetchAPI('/warp/link-kits');
export const mintLinkKit = (name: string): Promise<MintedLinkKit> =>
    fetchAPI('/warp/link-kits', { method: 'POST', body: JSON.stringify({ name }) });
export const revokeLinkKit = (linkId: string): Promise<{ success: boolean }> =>
    fetchAPI(`/warp/link-kits/${encodeURIComponent(linkId)}`, { method: 'DELETE' });

// Roll replaces the SECRET and nothing else: same link_id, same row, same peer.
// It is not a revoke - the machine keeps its tunnel until it is redeployed with
// the new key, at which point kill_old swaps the connection. Revoke is still
// the immediate-cutoff path for a key that leaked.
export const rollLinkKit = (linkId: string): Promise<MintedLinkKit> =>
    fetchAPI(`/warp/link-kits/${encodeURIComponent(linkId)}/roll`, { method: 'POST' });

// BYON node warp keys: the overlay credential a customer machine needs BEFORE it
// can enroll as a node. Separate product and separate cap from a link kit (max
// nodes, not max links); the two are kept apart server-side by a node_id prefix.
// `used` counts connected nodes AND unredeemed keys, matching what the mint
// endpoint enforces.
export interface NodeWarpKey {
    id: number;
    name: string;
    node_id: string;
    created_at: string;
}
export interface MintedNodeWarpKey {
    success: boolean;
    warp_key: string;
    node_id: string;
    note: string;
    message?: string;
}
export const listNodeWarpKeys = (): Promise<{ success: boolean; keys: NodeWarpKey[]; used?: number; limit?: number }> =>
    fetchAPI('/warp/node-keys');
export const mintNodeWarpKey = (name: string): Promise<MintedNodeWarpKey> =>
    fetchAPI('/warp/node-keys', { method: 'POST', body: JSON.stringify({ name }) });
export const revokeNodeWarpKey = (nodeId: string): Promise<{ success: boolean }> =>
    fetchAPI(`/warp/node-keys/${encodeURIComponent(nodeId)}`, { method: 'DELETE' });

// Same in-place roll as rollLinkKit. The node_id and the location name do not
// move, which is what keeps the tenant's servers pointing at the same machine:
// a server resolves through the node row, and the node identifies itself by
// NODE_ID, not by this key.
export const rollNodeWarpKey = (nodeId: string): Promise<MintedNodeWarpKey> =>
    fetchAPI(`/warp/node-keys/${encodeURIComponent(nodeId)}/roll`, { method: 'POST' });

// Live availability check for the route-create form. Accepts the same
// three input shapes the create endpoint does and answers `{available}`
// without leaking who owns a taken domain.
export interface DomainCheckRequest {
    domain?: string;
    subdomain?: string;
    hosterDomain?: string;
    customDomain?: string;
}
export interface DomainCheckResponse {
    available: boolean;
    domain?: string;
    reason?: string;
}
export const checkDomainAvailability = (req: DomainCheckRequest): Promise<DomainCheckResponse> => {
    const params = new URLSearchParams();
    if (req.domain) params.set('domain', req.domain);
    if (req.subdomain) params.set('subdomain', req.subdomain);
    if (req.hosterDomain) params.set('hosterDomain', req.hosterDomain);
    if (req.customDomain) params.set('customDomain', req.customDomain);
    return fetchAPI(`/gateway/check-domain?${params.toString()}`);
};

// --- BEAM SETTINGS ---
export interface BeamRelayInfo {
    beam_id: string;
    ip: string;
    private_ip?: string;
    public_host?: string;       // operator-configured (e.g. beam.dylaris.com)
    service_port: string;
    client_port?: string;
    download_port?: string;
    timestamp: number;
}
export interface BeamSettings {
    relayAddress: string;                // Effective (discovered or manual override)
    manualOverride?: string;             // Admin-configured override (empty = auto)
    discoveredRelays?: BeamRelayInfo[];  // Read-only list from Redis discovery
    bwLimit: number;
    enabled: boolean;
    downloadLink?: string;               // Optional CDN override
    minVersion?: string;                 // Force-update floor (empty = gating off)
}
export const getBeamSettings = (): Promise<{ success: boolean; settings?: BeamSettings }> => fetchAPI('/settings/beam');
export const saveBeamSettings = (data: BeamSettings) => fetchAPI('/settings/beam', { method: 'POST', body: JSON.stringify(data) });

// --- ADMIN API ---
export interface AdminServer {
    id: number;
    uuid: string;
    name: string;
    owner?: string;    // json:"owner" from models.Server.OwnerName
    node?: string;     // json:"node" from models.Server.NodeName
    nodeId?: number;
    status: string;
    activeSubServer?: string;
    createdAt?: string;
    serverType?: 'game' | 'proxy';
    memory?: number;
    diskLimit?: number;
    cpuLimit?: number;
    memberCount?: number;
    proxyId?: number | null;
    region?: string;
}
export interface DiskAnalysis {
    nodeOnline?: boolean;
    matched: { uuid: string; serverName: string; ownerName: string; status: string }[];
    orphaned: { uuid: string }[];
    missing: { id: number; uuid: string; serverName: string }[];
}
export const getAdminServers = (params?: { search?: string; orphaned?: boolean }) => {
    const q = new URLSearchParams();
    if (params?.search) q.set('search', params.search);
    if (params?.orphaned) q.set('orphaned', 'true');
    return fetchAPI(`/admin/servers${q.toString() ? '?' + q.toString() : ''}`);
};
export const getAdminDiskAnalysis = (nodeId: number): Promise<{ success: boolean } & DiskAnalysis> => fetchAPI(`/admin/nodes/${nodeId}/disk-analysis`);
export const updateServerOwner = (serverId: number, userId: string | null) => fetchAPI(`/admin/servers/${serverId}/owner`, { method: 'PATCH', body: JSON.stringify({ userId }) });
export const deleteOrphanedFolder = (nodeId: number, uuid: string) => fetchAPI(`/admin/nodes/${nodeId}/orphan?uuid=${encodeURIComponent(uuid)}`, { method: 'DELETE' });