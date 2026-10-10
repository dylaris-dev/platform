import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/**
 * Maintenance of the platform's own object storage: the orphan scan and the
 * bucket lifecycle rules. Platform storages only; a tenant's bucket is 404.
 */

export type OrphanKind = 'server-backup' | 'platform-backup' | 'migration-transfer' | 'probe';

export interface OrphanCandidate {
    key: string;
    size: number;
    lastModified: string;
    kind: OrphanKind;
    serverUuid?: string;
    /** Absent where the key names no server. */
    serverExists?: boolean;
    jobId?: number;
    /** Absent where the key names no job. */
    jobExists?: boolean;
}

export interface OrphanScan {
    candidates: OrphanCandidate[];
    candidateBytes: number;
    /**
     * Unreferenced files whose server and job still exist: what real backups
     * look like to a database that lost their rows (a restore, a second Core).
     * Kept out of `candidates`; deleting one needs includeLive.
     */
    live: OrphanCandidate[];
    liveBytes: number;
    referencedCount: number;
    referencedBytes: number;
    recentCount: number;
    recentBytes: number;
    unclassifiedCount: number;
    unclassifiedBytes: number;
    scannedAt: string;
}

export interface OrphanDeleteResult {
    key: string;
    deleted: boolean;
    size?: number;
    reason?: string;
}

export interface LifecycleRule {
    id: string;
    status: string;
    prefix: string;
    expirationDays?: number;
    abortMultipartDays?: number;
    ours: boolean;
}

export async function scanStorageOrphans(id: number): Promise<{ success: boolean; scan?: OrphanScan; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/storage/${id}/orphans`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function deleteStorageOrphans(
    id: number,
    keys: string[],
    includeLive = false,
): Promise<{ success: boolean; results?: OrphanDeleteResult[]; deleted?: number; deletedBytes?: number; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/storage/${id}/orphans/delete`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ keys, includeLive }),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function getStorageLifecycle(
    id: number,
): Promise<{ success: boolean; bucket?: string; rules?: LifecycleRule[]; planned?: LifecycleRule[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/storage/${id}/lifecycle`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function applyStorageLifecycle(
    id: number,
): Promise<{ success: boolean; bucket?: string; rules?: LifecycleRule[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/storage/${id}/lifecycle`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}
