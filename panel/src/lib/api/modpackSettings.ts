// typed client for /api/admin/settings/modpacks. GET omits the S3
// secret value (returns "") but exposes secretSet so the UI can render
// "(unchanged — leave empty)" vs "(not set yet)".

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface ModpackSettings {
    featureEnabled: boolean;
    // '' means nothing has been chosen yet, which is a real state: an install
    // that never configured modpack storage has no provider and no paths, and
    // preselecting "local paths" made an unconfigured backend look configured
    // right up until the first upload returned 424.
    provider: '' | 'local' | 's3';
    paths: string[];
    s3Endpoint: string;
    s3Bucket: string;
    s3Region: string;
    s3AccessKey: string;
    s3SecretKey?: string;
    updateCheckIntervalHours: number;
    shareLinksEnabled: boolean;
    // Core's own public origin. A node installing a pack built here downloads it
    // from {corePublicUrl}/mirror/, so an unset one means such a pack cannot be
    // installed on a server.
    corePublicUrl: string;
    // References a saved storage connection. When set (> 0), modpack storage is
    // built from that connection and the inline s3 fields are ignored.
    connectionId?: number;
    // The origin the settings request itself arrived on, offered as a one-click
    // suggestion for corePublicUrl. Read-only: sending it back changes nothing,
    // and Core never persists it on its own (see requestOrigin in Core - the
    // Host header is client-controlled, so it is a suggestion an admin accepts,
    // not a value that writes itself).
    detectedCorePublicUrl?: string;
}

export interface GetModpackSettingsResponse {
    success: boolean;
    settings?: ModpackSettings;
    secretSet?: boolean;
    message?: string;
}

export async function getModpackSettings(): Promise<GetModpackSettingsResponse> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/modpacks`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function setModpackSettings(s: ModpackSettings): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/modpacks`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(s),
        });
        return (await handleResponse(res)) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function setUserModpackFlag(userId: string, canCreate: boolean): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${encodeURIComponent(userId)}/modpack-flag`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ canCreate }),
        });
        return (await handleResponse(res)) as any;
    } catch (err) { return handleError(err) as any; }
}

// Stop overriding this user: clears the manual marker so they follow the platform
// "Open authoring to users" switch again. Does NOT change their current
// permission - it changes who decides it from here on.
export async function clearUserModpackOverride(userId: string): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${encodeURIComponent(userId)}/modpack-flag`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as any;
    } catch (err) { return handleError(err) as any; }
}
