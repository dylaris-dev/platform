// Panel API client for the unified pack builder surface: user-owned packs,
// their builds, and the content (mods / resource-packs / plugins) per build.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface Pack {
    id: number;
    ownerId: string;
    internalName: string;
    internalSlug: string;
    summary: string;
    modrinthProjectId: string;
    modrinthProjectName: string;
    modrinthVisibility: string;
    createdAt: string;
    updatedAt: string;
}

export interface PackBuild {
    id: number;
    packId: number;
    versionString: string;
    minecraft: string;
    loader: string;
    loaderVersion: string;
    channel: string;
    frozen: boolean;
    modrinthPublished: boolean;
    // Storage key of the rendered .mrpack once persisted (beta/release publish).
    // Empty on drafts — those are still installable, Core renders on the fly.
    mrpackStorageKey: string;
    createdAt: string;
}

export interface BuildContentEntry {
    id: number;            // modversion id
    modId: number;
    version: string;
    side: 'client' | 'server' | 'both';
    modSlug: string;
    prettyName: string;
    contentType: string;
    source: string;
    targetPath: string;
    modrinthProjectId: string;
    linked: boolean;
    modrinthVersionId: string;
    modrinthLatestVersionId: string;
    modrinthLastChecked: string | null;
}

async function get<T>(path: string, fallback: T, key: string): Promise<T> {
    try {
        const res = await fetch(`${API_URL}${path}`, { headers: getAuthHeader() });
        const data = await handleResponse(res);
        return ((data as any)[key] ?? fallback) as T;
    } catch (err) { handleError(err); return fallback; }
}

async function send(path: string, method: string, body?: unknown) {
    try {
        const res = await fetch(`${API_URL}${path}`, {
            method,
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: body === undefined ? undefined : JSON.stringify(body),
        });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export const listPacks = () => get<Pack[]>('/me/packs', [], 'packs');
export const createPack = (input: { name: string; slug?: string; summary?: string }) => send('/me/packs', 'POST', input);
export const getPack = (id: number) => get<Pack | null>(`/packs/${id}`, null, 'pack');
export const updatePack = (id: number, input: Partial<Pack>) => send(`/packs/${id}`, 'PATCH', input);
export const deletePack = (id: number) => send(`/packs/${id}`, 'DELETE');

export const listBuilds = (packId: number) => get<PackBuild[]>(`/packs/${packId}/builds`, [], 'builds');
export const createBuild = (packId: number, input: { versionString: string; minecraft?: string; loader?: string; loaderVersion?: string; changelog?: string }) => send(`/packs/${packId}/builds`, 'POST', input);
export const updateBuild = (packId: number, buildId: number, input: Partial<PackBuild>) => send(`/packs/${packId}/builds/${buildId}`, 'PATCH', input);
export const deleteBuild = (packId: number, buildId: number) => send(`/packs/${packId}/builds/${buildId}`, 'DELETE');

export const listContent = (packId: number, buildId: number) => get<BuildContentEntry[]>(`/packs/${packId}/builds/${buildId}/content`, [], 'content');
export const addModrinthContent = (packId: number, buildId: number, input: { projectId: string; versionId: string; side?: string; resolveDeps?: boolean; contentType?: string }) => send(`/packs/${packId}/builds/${buildId}/content/modrinth`, 'POST', input);
export const removeContent = (packId: number, buildId: number, modversionId: number) => send(`/packs/${packId}/builds/${buildId}/content/${modversionId}`, 'DELETE');
export const setContentSide = (packId: number, buildId: number, modversionId: number, side: string) => send(`/packs/${packId}/builds/${buildId}/content/${modversionId}/side`, 'PATCH', { side });

export async function uploadContent(packId: number, buildId: number, file: File, side: string, contentType: string) {
    try {
        const fd = new FormData();
        fd.append('file', file);
        fd.append('side', side);
        fd.append('contentType', contentType);
        const res = await fetch(`${API_URL}/packs/${packId}/builds/${buildId}/content/upload`, {
            method: 'POST',
            headers: getAuthHeader(), // no Content-Type: browser sets multipart boundary
            body: fd,
        });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

// Maps Modrinth's project_type vocabulary to our internal ContentType.
// "resourcepack" matches verbatim; "shader" -> "shaderpack"; anything else -> "mod".
export const modrinthTypeToContentType = (projectType: string): string => {
    switch (projectType) {
        case 'resourcepack': return 'resourcepack';
        case 'shader': return 'shaderpack';
        default: return 'mod';
    }
};
