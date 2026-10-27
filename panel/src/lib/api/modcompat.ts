import { API_URL, getAuthHeader, handleError, handleResponse } from '@/lib/api/core';

// Cross-Minecraft-version availability, shared by the modpack builder and a
// modded server. Both endpoints return the identical matrix shape, which is why
// one component renders either.

export type CompatSide = 'client' | 'server' | 'both';
export type CompatStatus = 'green' | 'orange' | 'red' | 'empty';
export type CompatMode = 'specific' | 'all-newer' | 'newer-lines';

export interface CompatBucket {
    total: number;
    available: number;
    status: CompatStatus;
}

export interface CompatMissing {
    key: string;
    projectId: string;
    title: string;
    slug: string;
    side: CompatSide;
    currentVersion: string;
    // 'no-version': the project is on Modrinth but has nothing for this game
    // version + loader. 'unresolvable': the project is not in the search index
    // at all (archived, unlisted, withdrawn), so this is "we cannot tell",
    // which is a different claim from "not available".
    reason: 'no-version' | 'unresolvable';
}

export interface CompatVersion {
    minecraft: string;
    status: CompatStatus;
    buckets: Record<CompatSide, CompatBucket>;
    missing: CompatMissing[];
}

export interface CompatLine {
    line: string;
    green: number;
    orange: number;
    red: number;
    versions: CompatVersion[];
}

export interface CompatMatrix {
    loader: string;
    current: string;
    mode: CompatMode;
    /** Items that carry a Modrinth identity and could be checked. */
    checked: number;
    /** Manual uploads: counted, never failed. Nothing is knowable about them. */
    unlinked: number;
    lines: CompatLine[];
}

export interface CompatResponse {
    success: boolean;
    matrix?: CompatMatrix;
    message?: string;
}

function compatQuery(mode: CompatMode, mc?: string): string {
    const q = new URLSearchParams({ mode });
    if (mc) q.set('mc', mc);
    return q.toString();
}

export async function getBuildCompat(
    packId: number,
    buildId: number,
    mode: CompatMode,
    mc?: string,
    loader?: string,
): Promise<CompatResponse> {
    try {
        const res = await fetch(
            `${API_URL}/packs/${packId}/builds/${buildId}/compat?${compatQuery(mode, mc)}${loader ? `&loader=${encodeURIComponent(loader)}` : ''}`,
            { headers: getAuthHeader() },
        );
        const data = await res.json();
        return { success: res.ok, ...data } as CompatResponse;
    } catch (err) { return handleError(err) as CompatResponse; }
}

export async function getServerCompat(
    serverId: number,
    mode: CompatMode,
    mc?: string,
): Promise<CompatResponse> {
    try {
        const res = await fetch(
            `${API_URL}/servers/${serverId}/mods/compat?${compatQuery(mode, mc)}`,
            { headers: getAuthHeader() },
        );
        const data = await res.json();
        return { success: res.ok, ...data } as CompatResponse;
    } catch (err) { return handleError(err) as CompatResponse; }
}

// --- Modpack migration -----------------------------------------------------

export interface MigratedItem {
    modversionId: number;
    title: string;
    version?: string;
    reason?: string;
}

export interface MigrateResponse {
    success: boolean;
    message?: string;
    build?: import('./packs').PackBuild;
    migrated?: MigratedItem[];
    uploads?: MigratedItem[];
    unavailable?: MigratedItem[];
    failed?: MigratedItem[];
}

export async function migrateBuild(
    packId: number,
    buildId: number,
    input: { minecraft: string; versionString: string; loader?: string; loaderVersion?: string; dropUnavailable?: boolean; changelog?: string },
): Promise<MigrateResponse> {
    try {
        const res = await fetch(`${API_URL}/packs/${packId}/builds/${buildId}/migrate`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        // 409 is the "some mods would be lost" gate, not a failure: it carries
        // the list the dialog asks the user to confirm.
        const data = await res.json();
        return { success: res.ok, ...data } as MigrateResponse;
    } catch (err) { return handleError(err) as MigrateResponse; }
}

// --- Server side -----------------------------------------------------------

export interface UnmanagedFile {
    directory: 'mods' | 'plugins';
    name: string;
    size: number;
}

// RAISES on a failed request.
//
// Core goes out of its way to keep these apart, and says why in its own comment
// at the handler: the node answers a MISSING directory with an empty list, so an
// error there is a real failure to look, and "reporting that as nothing
// unmanaged would hide exactly the thing this endpoint exists to reveal". It
// returns 502 for it.
//
// This wrapper then flattened the 502 back into an empty list, which put the
// guard on one side of the boundary only. A server whose node could not be
// reached read as a server with nothing out of place.
export async function getUnmanagedMods(serverId: number): Promise<UnmanagedFile[]> {
    const res = await fetch(`${API_URL}/servers/${serverId}/mods/unmanaged`, {
        headers: getAuthHeader(),
    });
    const data = await handleResponse(res);
    if (!(data as any)?.success) {
        throw new Error((data as any)?.message || 'Could not check this server for unknown jars.');
    }
    return (data as any).files || [];
}

export interface IdentifyResult {
    directory: string;
    name: string;
    matched: boolean;
    title?: string;
    projectId?: string;
    versionId?: string;
    reason?: string;
}

export async function identifyMods(
    serverId: number,
    files: { directory: string; name: string }[],
): Promise<{ success: boolean; linked?: number; results?: IdentifyResult[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/mods/identify`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ files }),
        });
        const data = await res.json();
        return { success: res.ok, ...data };
    } catch (err) { return handleError(err) as any; }
}

export interface UnavailableMod {
    modId: number;
    title: string;
    slug: string;
    currentVersionId?: string;
}

export interface VersionUpdateResponse {
    success: boolean;
    message?: string;
    installed?: number;
    removed?: UnavailableMod[];
    unavailable?: UnavailableMod[];
}

export async function updateServerVersion(
    serverId: number,
    input: { minecraft: string; installerVersion?: string; loader?: string; javaImage?: string; dropUnavailable?: boolean },
): Promise<VersionUpdateResponse> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/version-update`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        // 409 carries the mods that would be lost, same gate as migrateBuild.
        const data = await res.json();
        return { success: res.ok, ...data } as VersionUpdateResponse;
    } catch (err) { return handleError(err) as VersionUpdateResponse; }
}

export async function copySubServer(
    serverId: number,
    targetName: string,
): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/copy-sub-server`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ targetName }),
        });
        const data = await res.json();
        return { success: res.ok, ...data };
    } catch (err) { return handleError(err) as any; }
}
