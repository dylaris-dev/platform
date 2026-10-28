// Panel API client for the unified pack builder surface: user-owned packs,
// their builds, and the content (mods / resource-packs / plugins) per build.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface Pack {
	recommendedBuildId?: number;
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

export const setRecommendedBuild = (packId:number,buildId:number)=>send(`/packs/${packId}/recommended`,'PATCH',{buildId});
export async function forkPackBuild(packId:number,buildId:number,body:{targetPackId?:number;name?:string;slug?:string;versionString?:string},signal?:AbortSignal):Promise<{success:boolean;message?:string;pack?:Pack;build?:PackBuild}>{
    try {
        const response=await fetch(`${API_URL}/packs/${packId}/builds/${buildId}/fork`,{method:'POST',headers:{...getAuthHeader(),'Content-Type':'application/json'},body:JSON.stringify(body),signal});
        return await handleResponse(response) as any;
    } catch(error){if(signal?.aborted)return{success:false,message:'Copy cancelled.'};return handleError(error) as any;}
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
    modrinthVersionId?: string;
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
    modrinthVersionNumber?: string;
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
export const createPack = (input: { name: string; slug?: string; summary?: string; ownerId?: string }) => send('/me/packs', 'POST', input);

// --- Collaboration: packs other users share with me, and my invites ---

/** A pack someone else owns that the caller was invited to. */
export interface SharedPack extends Pack {
    ownerName: string;
    canEdit: boolean;
    canDelete: boolean;
}

export interface PacksOverview {
    packs: Pack[];
    shared: SharedPack[];
    /** Owners whose account the caller may create packs on. */
    createOn: { id: string; name: string }[];
}

export async function listPacksOverview(): Promise<PacksOverview> {
    const empty: PacksOverview = { packs: [], shared: [], createOn: [] };
    try {
        const res = await fetch(`${API_URL}/me/packs`, { headers: getAuthHeader() });
        const d = await handleResponse(res) as any;
        if (d?.success === false) return empty;
        return { packs: d.packs ?? [], shared: d.shared ?? [], createOn: d.createOn ?? [] };
    } catch (err) { handleError(err); return empty; }
}

export interface PackCollaborator {
    id: number;
    ownerId: string;
    ownerName: string;
    userId: string;
    username: string;
    /** null = every pack of the owner, including future ones. */
    packId: number | null;
    packName: string;
    canCreate: boolean;
    canEdit: boolean;
    canDelete: boolean;
    createdAt: string;
}

export interface PackCollaboratorInput {
    username: string;
    packIds: number[];
    canCreate: boolean;
    canEdit: boolean;
    canDelete: boolean;
}

export async function listPackCollaborators(): Promise<{ given: PackCollaborator[]; held: PackCollaborator[] }> {
    try {
        const res = await fetch(`${API_URL}/me/pack-collaborators`, { headers: getAuthHeader() });
        const d = await handleResponse(res) as any;
        return { given: d?.collaborators ?? [], held: d?.collaborations ?? [] };
    } catch (err) { handleError(err); return { given: [], held: [] }; }
}
export const invitePackCollaborator = (input: PackCollaboratorInput) => send('/me/pack-collaborators', 'POST', input);
export const updatePackCollaborator = (id: number, rights: { canCreate: boolean; canEdit: boolean; canDelete: boolean }) =>
    send(`/me/pack-collaborators/${id}`, 'PATCH', rights);
export const removePackCollaborator = (id: number) => send(`/me/pack-collaborators/${id}`, 'DELETE');

export interface PackAuditEvent {
    id: number;
    packId: number | null;
    packName: string;
    actorId: string;
    actorName: string;
    action: string;
    detail: Record<string, unknown>;
    createdAt: string;
}
/** A pack's history (packId) or the history of all of the caller's packs. */
export const getPackAudit = (packId?: number) =>
    get<PackAuditEvent[]>(packId ? `/packs/${packId}/audit` : '/me/pack-audit', [], 'events');
export const getPack = (id: number) => get<Pack | null>(`/packs/${id}`, null, 'pack');
export interface PackPermissions { canEdit: boolean; canDelete: boolean; isOwner: boolean }
export async function getPackDetails(id: number): Promise<{ pack: Pack | null; permissions: PackPermissions }> {
    const empty = { pack: null, permissions: { canEdit: false, canDelete: false, isOwner: false } };
    try {
        const res = await fetch(`${API_URL}/packs/${id}`, { headers: getAuthHeader() });
        const data = await handleResponse(res) as any;
        if (data?.success === false || !data?.pack) return empty;
        return { pack: data.pack, permissions: {
            canEdit: data.permissions?.canEdit === true,
            canDelete: data.permissions?.canDelete === true,
            isOwner: data.permissions?.isOwner === true,
        } };
    } catch (err) { handleError(err); return empty; }
}
export const updatePack = (id: number, input: Partial<Pack>) => send(`/packs/${id}`, 'PATCH', input);
export const deletePack = (id: number) => send(`/packs/${id}`, 'DELETE');

export const listBuilds = (packId: number) => get<PackBuild[]>(`/packs/${packId}/builds`, [], 'builds');
export const createBuild = (packId: number, input: { versionString: string; minecraft?: string; loader?: string; loaderVersion?: string; changelog?: string; sourceBuildId?: number }) => send(`/packs/${packId}/builds`, 'POST', input);
export const updateBuild = (packId: number, buildId: number, input: Partial<PackBuild>) => send(`/packs/${packId}/builds/${buildId}`, 'PATCH', input);
export const deleteBuild = (packId: number, buildId: number, confirmVersion?: string) => send(`/packs/${packId}/builds/${buildId}`, 'DELETE', confirmVersion === undefined ? undefined : { confirmVersion });

export const listContent = (packId: number, buildId: number) => get<BuildContentEntry[]>(`/packs/${packId}/builds/${buildId}/content`, [], 'content');
export const addModrinthContent = (packId: number, buildId: number, input: { projectId: string; versionId: string; side?: string; resolveDeps?: boolean; contentType?: string }) => send(`/packs/${packId}/builds/${buildId}/content/modrinth`, 'POST', input);
export const removeContent = (packId: number, buildId: number, modversionId: number) => send(`/packs/${packId}/builds/${buildId}/content/${modversionId}`, 'DELETE');
export const setContentSide = (packId: number, buildId: number, modversionId: number, side: string) => send(`/packs/${packId}/builds/${buildId}/content/${modversionId}/side`, 'PATCH', { side });

export async function uploadContent(packId: number, buildId: number, file: File, side: string, contentType: string, options?: { targetPath?: string; archive?: boolean }) {
    try {
        const fd = new FormData();
        fd.append('file', file);
        fd.append('side', side);
        fd.append('contentType', contentType);
        if (options?.targetPath) fd.append('targetPath', options.targetPath);
        if (options?.archive) fd.append('archive', 'true');
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
