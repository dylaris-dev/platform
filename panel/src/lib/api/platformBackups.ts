import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/**
 * The platform's OWN backups, as opposed to a game server's.
 *
 * A platform bundle is the installation: the database, and with it every node
 * secret, every storage credential and every user row. It is sealed under a
 * passphrase the operator sets once and writes down, so a downloaded bundle can
 * be restored onto a different Dylaris - and so it survives a CLUSTER_SECRET
 * rotation, which a backup tied to that secret would not.
 */

/** How servers are chosen. Always whole servers, never per sub-server. */
export type PlatformBackupServerMode = 'none' | 'list' | 'owner' | 'all' | 'byon';

export interface PlatformBackupServers {
    mode: PlatformBackupServerMode;
    /** Required by, and only read for, mode "owner". */
    ownerId?: string;
    /**
     * Required by, and only read for, mode "list".
     *
     * A selection outlives what it names: an id here pointing at a server that
     * has since been deleted is skipped and recorded, never an error.
     */
    serverIds?: number[];
}

export interface PlatformBackupSelection {
    database: boolean;
    metricsDb: boolean;
    library: boolean;
    modpacks: boolean;
    servers: PlatformBackupServers;
}

export interface PlatformBackupJob {
    id: number;
    name: string;
    schedule: string;
    selection: PlatformBackupSelection;
    storageId?: number;
    retentionCount: number;
    enabled: boolean;
    lastRunAt?: string;
    nextRunAt?: string;
    createdAt: string;
}

/** What a run actually did, per part - which is not what it was asked to do. */
export interface PlatformBackupComponent {
    kind: 'database' | 'metrics' | 'library' | 'modpacks' | 'server' | string;
    ref?: string;
    status: 'included' | 'skipped' | 'failed';
    sizeBytes?: number;
    message?: string;
}

export interface PlatformBackupRun {
    id: number;
    jobId: number;
    startedAt: string;
    completedAt?: string;
    status: 'running' | 'success' | 'failed';
    sizeBytes: number;
    storageKey: string;
    storageId?: number;
    errorMessage?: string;
    components: PlatformBackupComponent[];
}

/** A server a selection may name. */
export interface BackupTargetServer {
    id: number;
    uuid: string;
    name: string;
    ownerId: string;
    nodeId: number;
    /**
     * The NODE belongs to a customer. Ownership of the server says who uses it;
     * ownership of the node says whose machine it runs on.
     */
    byon: boolean;
}

export const EMPTY_SELECTION: PlatformBackupSelection = {
    database: false,
    metricsDb: false,
    library: false,
    modpacks: false,
    servers: { mode: 'none' },
};

export async function listPlatformBackupJobs(): Promise<{ success: boolean; jobs?: PlatformBackupJob[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export interface PlatformBackupJobInput {
    name: string;
    schedule: string;
    selection: PlatformBackupSelection;
    storageId: number | null;
    retentionCount: number;
    enabled: boolean;
}

export async function createPlatformBackupJob(
    input: PlatformBackupJobInput,
): Promise<{ success: boolean; job?: PlatformBackupJob; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function updatePlatformBackupJob(
    id: number,
    input: PlatformBackupJobInput,
): Promise<{ success: boolean; job?: PlatformBackupJob; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs/${id}`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function deletePlatformBackupJob(id: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function runPlatformBackupJob(id: number): Promise<{ success: boolean; runId?: number; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs/${id}/run`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function listPlatformBackupRuns(
    id: number,
): Promise<{ success: boolean; runs?: PlatformBackupRun[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/jobs/${id}/runs`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function listPlatformBackupTargets(): Promise<{
    success: boolean;
    targets?: BackupTargetServer[];
    /** "All BYON servers" is only meaningful on a store-connected platform. */
    byonOffered?: boolean;
    message?: string;
}> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/targets`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function getPlatformBackupPassphraseStatus(): Promise<{ success: boolean; isSet?: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/passphrase`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/**
 * Sets the backup passphrase.
 *
 * `replace` must be sent when one already exists, and it is not a formality:
 * bundles already written keep the OLD passphrase and nothing re-encrypts them.
 */
export async function setPlatformBackupPassphrase(
    passphrase: string,
    replace = false,
): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/passphrase`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ passphrase, replace }),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/** Where a finished bundle is downloaded from. Streamed through Core. */
export function platformBackupDownloadUrl(runId: number): string {
    return `${API_URL}/platform-backups/runs/${runId}/download`;
}

// ───────────── Reading a bundle back ─────────────

/** What a restore is allowed to put back. Chosen per restore, not per bundle. */
export interface PlatformRestoreSelection {
    database: boolean;
    library: boolean;
    modpacks: boolean;
}

/** The empty database a restored dump goes into. Never the live one. */
export interface RestoreTarget {
    host: string;
    port: string;
    user: string;
    password: string;
    dbName: string;
    sslMode: string;
}

export const EMPTY_TARGET: RestoreTarget = {
    host: '', port: '5432', user: '', password: '', dbName: '', sslMode: 'disable',
};

export interface BundleInspection {
    success: boolean;
    /** The release that wrote it. Readable without the passphrase. */
    source?: string;
    createdAt?: string;
    schema?: number;
    /** True only when the passphrase actually opened it. */
    passphraseOk?: boolean;
    selection?: PlatformBackupSelection;
    components?: PlatformBackupComponent[];
    /**
     * Whether the bundle carries the source installation's cluster secret. Without
     * it the restored credentials cannot be re-encrypted for this installation,
     * so nodes have to re-pair and storage credentials be entered again.
     */
    carriesClusterSecret?: boolean;
    message?: string;
}

export interface PlatformRestoreResult {
    source: string;
    createdAt: string;
    components: PlatformBackupComponent[];
    reseal?: { buckets: { name: string; moved: number; unreadable: number }[] };
    warnings?: string[];
}

/** Identifies a bundle. The passphrase is optional: without it only the header is read. */
export async function inspectBundle(file: File, passphrase?: string): Promise<BundleInspection> {
    try {
        const form = new FormData();
        form.append('bundle', file);
        if (passphrase) form.append('passphrase', passphrase);
        const res = await fetch(`${API_URL}/platform-backups/inspect`, {
            method: 'POST', headers: getAuthHeader(), body: form,
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e) as BundleInspection;
    }
}

export interface RestoreRequest {
    passphrase: string;
    components: PlatformRestoreSelection;
    target: RestoreTarget;
    /** Confirms a target database that already holds tables. */
    overwrite: boolean;
}

export async function restoreBundle(
    file: File, req: RestoreRequest,
): Promise<{ success: boolean; result?: PlatformRestoreResult; message?: string }> {
    try {
        const form = new FormData();
        form.append('bundle', file);
        form.append('request', JSON.stringify(req));
        const res = await fetch(`${API_URL}/platform-backups/restore`, {
            method: 'POST', headers: getAuthHeader(), body: form,
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/** Restores a run this instance already wrote, without carrying it through an upload. */
export async function restorePlatformBackupRun(
    runId: number, req: RestoreRequest,
): Promise<{ success: boolean; result?: PlatformRestoreResult; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/platform-backups/runs/${runId}/restore`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(req),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}
