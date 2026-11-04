import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/**
 * A tenant's OWN backup storage.
 *
 * Separate from the admin `backup-storages` client on purpose: these routes are
 * anchored to the caller's own id and can only ever see or write their rows,
 * where the admin ones are platform-wide. Two clients rather than one with a
 * flag, so a call cannot accidentally be pointed at the wrong surface.
 */

export interface OwnBackupStorage {
    id: number;
    name: string;
    provider: string;
    config: S3Config;
    /** This tenant's default, used by every job that does not pick a storage. */
    isDefault: boolean;
    /** True when a secret is stored. The secret itself never leaves Core. */
    secretSet?: boolean;
    createdAt: string;
    /** Most a backup may upload to it, in Mbit/s; null is no cap. */
    uploadCapMbit?: number | null;
}

export interface S3Config {
    endpoint?: string;
    region?: string;
    bucket?: string;
    prefix?: string;
    accessKeyId?: string;
    /** Write-only: sent on save, never returned. Blank on an edit keeps the stored one. */
    secretAccessKey?: string;
    forcePathStyle?: boolean;
}

export interface OwnStorageInput {
    name: string;
    config: S3Config;
    /**
     * Omitted by the Storage page: where backups go is chosen through
     * putStoragePrefs only. Core leaves the flag and the backups choice alone
     * on an edit that does not carry it, so a stale form cannot move them.
     */
    isDefault?: boolean;
    /** Sent on every save: Core reads an absent cap as "no cap". */
    uploadCapMbit: number | null;
}

export async function listOwnBackupStorages(): Promise<{ success: boolean; status?: number; storages?: OwnBackupStorage[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/backup-storages`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function createOwnBackupStorage(input: OwnStorageInput) {
    return send('POST', `${API_URL}/me/backup-storages`, input);
}

export async function updateOwnBackupStorage(id: number, input: OwnStorageInput) {
    return send('PATCH', `${API_URL}/me/backup-storages/${id}`, input);
}

export async function deleteOwnBackupStorage(id: number) {
    try {
        const res = await fetch(`${API_URL}/me/backup-storages/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function testOwnBackupStorage(id: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/backup-storages/${id}/test`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

// The provider is fixed rather than a field: s3 is the only thing an account may
// connect. Core refuses anything else, and offering a choice the server rejects
// would be a form that can be filled in wrong.
async function send(method: 'POST' | 'PATCH', url: string, input: OwnStorageInput) {
    try {
        const res = await fetch(url, {
            method,
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: input.name, provider: 's3', config: input.config, isDefault: input.isDefault, uploadCapMbit: input.uploadCapMbit }),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/**
 * What a form must carry before it can be saved.
 *
 * The secret is required on CREATE and optional on EDIT: the form never receives
 * the stored one, so a blank field on an edit means "keep it". Core refuses that
 * blank when the endpoint, bucket or access key moved, because reusing a
 * credential against a target it was not issued for is credential rebinding
 * rather than convenience - this only spares the round trip for the obvious case.
 */
export function ownStorageIncomplete(input: OwnStorageInput, isEdit: boolean): string | null {
    const c = input.config;
    if (!input.name.trim()) return 'Give this storage a name';
    if (!c.endpoint?.trim()) return 'The endpoint is required';
    if (!c.bucket?.trim()) return 'The bucket is required';
    if (!c.accessKeyId?.trim()) return 'The access key ID is required';
    if (!isEdit && !c.secretAccessKey?.trim()) return 'The secret access key is required';
    return null;
}

/**
 * Where NEW backups and templates go: null is Dylaris storage, a number one of
 * the caller's own targets. Switching never moves what already exists.
 */
export interface StoragePrefs {
    backupsStorageId: number | null;
    templatesStorageId: number | null;
}

export type StorageFeature = 'backups' | 'templates';

export async function getStoragePrefs(): Promise<{ success: boolean; message?: string } & Partial<StoragePrefs>> {
    try {
        const res = await fetch(`${API_URL}/me/storage-prefs`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function putStoragePrefs(patch: Partial<StoragePrefs>): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/storage-prefs`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(patch),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/**
 * The body for one select: only that feature's field, so saving one choice can
 * never overwrite the other. The select's empty value is Dylaris storage.
 */
export function storagePrefPayload(feature: StorageFeature, value: string): Partial<StoragePrefs> {
    const id = value === '' ? null : Number(value);
    return feature === 'backups' ? { backupsStorageId: id } : { templatesStorageId: id };
}

/** Where a template lives, for its badge. An id not among the caller's own is the owner's (a shared template).
 *  The platform's storage is "Dylaris storage" only on the SaaS (features.hosted). */
export function storageLabel(id: number | null | undefined, own: { id: number; name: string }[], hosted: boolean): string {
    if (id == null) return hosted ? 'Dylaris storage' : 'Platform storage';
    return own.find(s => s.id === id)?.name ?? "Owner's own storage";
}

/** A backup job's storage choices: the platform's, then the caller's own targets.
 *  With own storage off only `current` (the job's stored value) is kept among
 *  the own targets: Core accepts an unchanged one so an old job stays editable,
 *  and refuses any other. */
export function jobStorageOptions(
    platform: { id: number; name: string; provider: string }[],
    own: { id: number; name: string }[],
    ownAllowed = true,
    current: number | null = null,
): { id: number; label: string }[] {
    return [
        ...platform.map(s => ({ id: s.id, label: `${s.name} (${s.provider})` })),
        ...own.filter(s => ownAllowed || s.id === current).map(s => ({ id: s.id, label: `Own: ${s.name}` })),
    ];
}

/** A user's own storage target as an admin sees it: never the credentials. */
export interface AdminStorageTarget {
    id: number;
    name: string;
    provider: string;
    endpointHost: string;
    bucket: string;
    prefix: string;
    isDefault: boolean;
    createdAt: string;
    /** What still names it. A delete is refused while any count is above zero. */
    usage: { jobs: number; runs: number; templates: number; prefs: number };
}

export async function adminListUserStorageTargets(userId: string): Promise<{ success: boolean; storages?: AdminStorageTarget[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/storage-targets`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

export async function adminDeleteUserStorageTarget(userId: string, id: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/storage-targets/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}

/** The usage counts as one line, or null when nothing names the target. */
export function storageTargetUsage(u: AdminStorageTarget['usage']): string | null {
    const parts = [
        [u.jobs, 'backup schedule'],
        [u.runs, 'backup'],
        [u.templates, 'template'],
        [u.prefs, 'storage choice'],
    ].filter(([n]) => (n as number) > 0).map(([n, label]) => `${n} ${label}${n === 1 ? '' : 's'}`);
    return parts.length ? parts.join(', ') : null;
}
