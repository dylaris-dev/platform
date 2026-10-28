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
    /** "modrinth" for a version imported from the pack's Modrinth project. */
    importedFrom?: string;
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

// --- The pack's Modrinth project page (edited through the owner's token) ---

export interface ModrinthNag { kind: string; severity: 'required' | 'warning' | 'suggestion' | string; details?: Record<string, unknown> }
export interface ModrinthProjectPage {
    id: string; slug: string; title: string; description: string; body: string;
    status: string; requested_status: string | null;
    categories: string[]; additional_categories: string[];
    icon_url: string | null; source_url: string | null; issues_url: string | null; wiki_url: string | null; discord_url: string | null;
    versions: string[];
    license: { id: string; url: string | null };
    moderator_message: { message: string; body: string | null } | null;
}
export interface ModrinthPageResult { success: boolean; message?: string; project?: ModrinthProjectPage; nags?: ModrinthNag[]; environment?: string }
export interface ModrinthPageEdit {
    title?: string; description?: string; body?: string;
    categories?: string[]; additionalCategories?: string[];
    licenseId?: string; licenseUrl?: string;
    sourceUrl?: string; issuesUrl?: string; wikiUrl?: string; discordUrl?: string;
    environment?: string;
}

export async function getModrinthPage(packId: number): Promise<ModrinthPageResult> {
    try { return handleResponse(await fetch(`${API_URL}/packs/${packId}/modrinth`, { headers: getAuthHeader() })) as any; }
    catch (err) { return handleError(err) as any; }
}
export const editModrinthPage = (packId: number, edit: ModrinthPageEdit): Promise<ModrinthPageResult> => send(`/packs/${packId}/modrinth`, 'PATCH', edit);
export const submitModrinthReview = (packId: number): Promise<ModrinthPageResult> => send(`/packs/${packId}/modrinth/submit`, 'POST');
export async function uploadModrinthIcon(packId: number, file: File): Promise<ModrinthPageResult> {
    try {
        const fd = new FormData();
        fd.append('icon', file);
        const res = await fetch(`${API_URL}/packs/${packId}/modrinth/icon`, { method: 'POST', headers: getAuthHeader(), body: fd });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

// --- Importing existing Modrinth modpacks ---

export interface ModrinthImportProject {
    id: string; slug: string; title: string; description: string; status: string;
    icon_url: string | null; versions: string[];
    hidden: boolean; packId?: number;
}
export interface ModrinthImportVersion { id: string; versionNumber: string; versionType: string; gameVersions: string[]; loaders: string[]; datePublished: string; buildId?: number }
export interface ImportVersionResult { success: boolean; message?: string; buildId?: number; linked?: number; uploaded?: number; warnings?: string[]; existing?: boolean }

export async function listModrinthImportProjects(): Promise<{ success: boolean; message?: string; projects?: ModrinthImportProject[] }> {
    try { return handleResponse(await fetch(`${API_URL}/me/modrinth/projects`, { headers: getAuthHeader() })) as any; }
    catch (err) { return handleError(err) as any; }
}
export const setModrinthProjectHidden = (projectId: string, hidden: boolean) => send(`/me/modrinth/projects/${encodeURIComponent(projectId)}/hidden`, 'PUT', { hidden });
export const importModrinthProject = (projectId: string): Promise<{ success: boolean; message?: string; pack?: Pack; existing?: boolean }> => send(`/me/modrinth/projects/${encodeURIComponent(projectId)}/import`, 'POST');
export async function listModrinthPackVersions(packId: number): Promise<{ success: boolean; message?: string; versions?: ModrinthImportVersion[] }> {
    try { return handleResponse(await fetch(`${API_URL}/packs/${packId}/modrinth/versions`, { headers: getAuthHeader() })) as any; }
    catch (err) { return handleError(err) as any; }
}
export const importModrinthPackVersion = (packId: number, versionId: string): Promise<ImportVersionResult> => send(`/packs/${packId}/modrinth/versions/${encodeURIComponent(versionId)}/import`, 'POST');
