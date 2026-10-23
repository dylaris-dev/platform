// Modrinth proxy client + installed-mods CRUD. All requests go
// through the core proxy at /api/modrinth/* so we get caching + CORS for
// free. Types mirror Modrinth's API shape (only the fields we use).

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface ModrinthSearchHit {
    project_id: string;
    project_type: string;
    slug: string;
    author: string;
    title: string;
    description: string;
    icon_url: string;
    downloads: number;
    follows: number;
    categories: string[];
    versions: string[];
    latest_version?: string;
    client_side?: string;
    server_side?: string;
}

export interface ModrinthSearchResult {
    hits: ModrinthSearchHit[];
    offset: number;
    limit: number;
    total_hits: number;
}

export interface ModrinthProject {
    id: string;
    slug: string;
    title: string;
    description: string;
    body: string;
    icon_url?: string;
    project_type: string;
    // Modrinth per-project client support: "required" | "optional" | "unsupported"
    // | "unknown". Passed through verbatim by the Core proxy; drives the
    // client-side-required install warning.
    client_side?: 'required' | 'optional' | 'unsupported' | 'unknown';
    server_side?: 'required' | 'optional' | 'unsupported' | 'unknown';
    categories: string[];
    additional_categories: string[];
    loaders: string[];
    game_versions: string[];
    versions: string[];
    license?: { id: string; name: string; url?: string };
    source_url?: string;
    issues_url?: string;
    wiki_url?: string;
    discord_url?: string;
    downloads: number;
    followers: number;
    gallery?: { url: string; title?: string; description?: string }[];
}

export interface ModrinthVersionFile {
    hashes: { sha512?: string; sha1?: string };
    url: string;
    filename: string;
    primary: boolean;
    size: number;
}

export interface ModrinthVersion {
    id: string;
    project_id: string;
    name: string;
    version_number: string;
    changelog?: string;
    game_versions: string[];
    loaders: string[];
    version_type: 'release' | 'beta' | 'alpha';
    featured: boolean;
    date_published: string;
    files: ModrinthVersionFile[];
    dependencies?: { version_id?: string; project_id?: string; dependency_type: string }[];
}

interface SearchParams {
    query?: string;
    loaders?: string[];
    versions?: string[];
    categories?: string[];
    projectType?: 'mod' | 'plugin' | 'modpack' | 'shader' | 'resourcepack' | 'datapack';
    // client|server|both: Modrinth's Environment filter (pack builder);
    // server_only|client_server|server_runs: the server Content tab's buckets.
    side?: 'server' | 'client' | 'both' | 'server_only' | 'client_server' | 'server_runs';
    limit?: number;
    offset?: number;
    index?: 'relevance' | 'downloads' | 'follows' | 'newest' | 'updated';
}

function buildSearchURL(params: SearchParams): string {
    const u = new URLSearchParams();
    if (params.query) u.set('query', params.query);
    if (params.loaders?.length) u.set('loaders', params.loaders.join(','));
    if (params.versions?.length) u.set('versions', params.versions.join(','));
    if (params.categories?.length) u.set('categories', params.categories.join(','));
    if (params.projectType) u.set('project_type', params.projectType);
    if (params.side) u.set('side', params.side);
    if (params.limit) u.set('limit', String(params.limit));
    if (params.offset) u.set('offset', String(params.offset));
    if (params.index) u.set('index', params.index);
    return u.toString();
}

// null means the search FAILED (rate limit outlasted the retries, Core or
// Modrinth down); an empty `hits` means it ran and found nothing. Callers must
// not show the second message for the first.
export async function searchModrinth(params: SearchParams): Promise<ModrinthSearchResult | null> {
    try {
        const res = await fetchRetryingRateLimit(`${API_URL}/modrinth/search?${buildSearchURL(params)}`, {
            headers: getAuthHeader(),
        });
        if (!res.ok) return null;
        return await res.json();
    } catch { return null; }
}

export async function getModrinthProject(slug: string): Promise<ModrinthProject | null> {
    try {
        const res = await fetch(`${API_URL}/modrinth/project/${encodeURIComponent(slug)}`, {
            headers: getAuthHeader(),
        });
        if (!res.ok) return null;
        return res.json();
    } catch { return null; }
}

const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms));

/**
 * fetch that waits out a rate limit instead of reporting it as an answer.
 *
 * Core answers 429 for its own per-IP bucket and 503 with Retry-After while
 * Modrinth has it cooling down. Either way the request was never judged, so a
 * caller that read it as "no versions" would call a mod unchecked - or worse,
 * current. Retry-After is honoured, capped so one header cannot park the tab.
 */
export async function fetchRetryingRateLimit(
    url: string,
    init: RequestInit,
    opts: { retries?: number; maxWaitMs?: number; wait?: (ms: number) => Promise<void> } = {},
): Promise<Response> {
    const retries = opts.retries ?? 3;
    const maxWait = opts.maxWaitMs ?? 60_000;
    const wait = opts.wait ?? sleep;
    for (let attempt = 0; ; attempt++) {
        const res = await fetch(url, init);
        const limited = res.status === 429 || (res.status === 503 && res.headers.has('Retry-After'));
        if (!limited || attempt >= retries) return res;
        const secs = Number(res.headers.get('Retry-After'));
        const ms = Number.isFinite(secs) && secs > 0 ? secs * 1000 : 2000 * 2 ** attempt;
        await wait(Math.min(ms, maxWait));
    }
}

export async function getModrinthVersions(
    slug: string,
    filter?: { loaders?: string[]; versions?: string[] },
): Promise<ModrinthVersion[]> {
    try {
        const q = new URLSearchParams();
        if (filter?.loaders?.length) q.set('loaders', filter.loaders.join(','));
        if (filter?.versions?.length) q.set('versions', filter.versions.join(','));
        const qs = q.toString();
        const url = `${API_URL}/modrinth/project/${encodeURIComponent(slug)}/versions${qs ? `?${qs}` : ''}`;
        const res = await fetchRetryingRateLimit(url, { headers: getAuthHeader() });
        if (!res.ok) return [];
        return res.json();
    } catch { return []; }
}

// --- Bulk lookups (Update all) ---
//
// One request per mod is what Update all used to cost, and Core is one IP for
// every customer against Modrinth's per-IP limit. These answer a whole server
// in a handful of calls. They THROW on failure: an empty answer means "Modrinth
// has nothing", which is a different statement from "we never asked".

// Core's caps per call; chunked here so a big server is still a few calls.
const BULK_IDS = 100;
const BULK_HASHES = 1000;

function chunks<T>(xs: T[], n: number): T[][] {
    const out: T[][] = [];
    for (let i = 0; i < xs.length; i += n) out.push(xs.slice(i, i + n));
    return out;
}

async function bulkGet<T>(path: string, ids: string[]): Promise<T[]> {
    const out: T[] = [];
    for (const part of chunks([...new Set(ids)], BULK_IDS)) {
        const res = await fetchRetryingRateLimit(`${API_URL}${path}?ids=${encodeURIComponent(part.join(','))}`, { headers: getAuthHeader() });
        if (!res.ok) throw new Error(`Modrinth lookup failed (${res.status})`);
        out.push(...await res.json());
    }
    return out;
}

/** Versions by id. Ids Modrinth does not know are absent from the result. */
export function getModrinthVersionsByIds(ids: string[]): Promise<ModrinthVersion[]> {
    return ids.length ? bulkGet<ModrinthVersion>('/modrinth/versions', ids) : Promise.resolve([]);
}

export type ModrinthProjectName = Pick<ModrinthProject, 'id' | 'slug' | 'title' | 'icon_url' | 'client_side' | 'server_side'>;

/** Project names by id (Core slims these to id, slug, title, icon, type, client_side, server_side). */
export function getModrinthProjectsByIds(ids: string[]): Promise<ModrinthProjectName[]> {
    return ids.length ? bulkGet<ModrinthProjectName>('/modrinth/projects', ids) : Promise.resolve([]);
}

/**
 * For each sha512, the newest version of that file's project for the loader,
 * Minecraft version and channels given. A hash with no such version is absent.
 */
export async function getLatestVersionsFromHashes(
    hashes: string[],
    filter: { loaders: string[]; gameVersions: string[]; versionTypes: ModrinthVersion['version_type'][] },
): Promise<Record<string, ModrinthVersion>> {
    const out: Record<string, ModrinthVersion> = {};
    for (const part of chunks([...new Set(hashes)], BULK_HASHES)) {
        const res = await fetchRetryingRateLimit(`${API_URL}/modrinth/version-files/update`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({
                hashes: part,
                algorithm: 'sha512',
                loaders: filter.loaders,
                game_versions: filter.gameVersions,
                version_types: filter.versionTypes,
            }),
        });
        if (!res.ok) throw new Error(`Modrinth update check failed (${res.status})`);
        Object.assign(out, await res.json());
    }
    return out;
}

// --- Category tags (Content-tab sidebar) ---

export interface ModrinthCategory {
    icon: string;         // inline SVG markup
    name: string;         // machine name, e.g. "optimization"
    project_type: string; // "mod" | "plugin" | "resourcepack" | "shader" | ...
    header: string;       // grouping header, e.g. "categories" | "resolutions"
}

// Modrinth's category tag list, proxied + day-cached by Core. Fail-open to []
// so the tab still works (without the category sidebar) if it can't load.
export async function getModrinthCategories(): Promise<ModrinthCategory[]> {
    try {
        const res = await fetch(`${API_URL}/modrinth/categories`, { headers: getAuthHeader() });
        if (!res.ok) return [];
        return res.json();
    } catch { return []; }
}

// --- Installed mods per server ---

export interface InstalledMod {
    id: number;
    serverId: number;
    subServerName: string;
    modrinthProjectId: string;
    modrinthProjectSlug: string;
    modrinthVersionId: string;
    title: string;
    fileName: string;
    sha512: string;
    installedAt: string;
    installedBy?: string;
    /**
     * What the NODE reported, not what the panel hoped for.
     *
     * An install is queued work, so this row exists before the node has done
     * anything: "installing" until it answers, then "installed" or "failed".
     * Rows written before the node reported at all read as "installed", which
     * is what they were always taken to be.
     */
    status?: 'installing' | 'installed' | 'failed';
    /** The node's reason for a failure. Empty otherwise. */
    statusMessage?: string;
    /** Kept out of "Update all". Updating it by hand still works. */
    updatesPaused?: boolean;
}

// RAISES on a failed request, unlike getServerModpackContents below, which
// fails open on purpose - and the difference between them is the point. The
// modpack snapshot decorates rows; this list is what the tab uses to decide
// whether a mod is already there. Flattened to [], the Installed section reads
// "No mods installed", every browse row loses its installed badge, and the
// obvious next move is to install something that is already on the server.
export async function listInstalledMods(serverId: number): Promise<InstalledMod[]> {
    const res = await fetch(`${API_URL}/servers/${serverId}/mods`, { headers: getAuthHeader() });
    const data = await handleResponse(res);
    if (!(data as any)?.success) {
        // The status rides along so a caller can tell "not allowed" (403) apart.
        throw Object.assign(new Error((data as any)?.message || 'Could not load the installed mods.'), { status: (data as any)?.status });
    }
    return (data as any).mods || [];
}

export interface InstallModPayload {
    projectId: string;
    projectSlug: string;
    versionId: string;
    title: string;
    fileName: string;
    downloadUrl: string;
    sha512?: string;
    targetDir?: 'mods' | 'plugins';
}

// status is what Core recorded: "installing" when the node will report the
// outcome, "installed" when it is too old to, and absent from an older Core.
export async function installMod(serverId: number, payload: InstallModPayload): Promise<{ success: boolean; message?: string; status?: 'installing' | 'installed' }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/mods`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export interface ModPlanEntry {
    projectId: string;
    slug: string;
    title: string;
    versionId: string;
    versionNumber: string;
    fileName: string;
    downloadUrl: string;
}

export interface ModPlanProblem {
    projectId: string;
    title: string;
    reason: string;
}

/** Core's dependency plan: `install` is dependency-first and ends with the requested build. */
export interface ModInstallPlan {
    install: ModPlanEntry[];
    unresolved: ModPlanProblem[];
    incompatible: ModPlanProblem[];
}

// Read-only on Core's side. A 4xx means Core refused the build or the caller
// (the install would be refused the same way); anything else means the plan
// could not be worked out, which is not the same as "no dependencies".
export async function planModInstall(
    serverId: number,
    versionId: string,
): Promise<{ ok: true; plan: ModInstallPlan } | { ok: false; status?: number; message: string }> {
    try {
        // Rate-limit aware: Update all plans every mod it updates, and the plan
        // shares Core's 30/min Modrinth bucket with the update check before it.
        const res = await fetchRetryingRateLimit(`${API_URL}/servers/${serverId}/mods/plan`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ versionId }),
        });
        const data = await handleResponse(res);
        // A 401 that ends the session comes back without a status; the caller
        // needs it to stop rather than offer an install without dependencies.
        if (!data.success) return { ok: false, status: data.status ?? res.status, message: data.message || 'Could not work out the dependencies.' };
        return {
            ok: true,
            plan: { install: data.install ?? [], unresolved: data.unresolved ?? [], incompatible: data.incompatible ?? [] },
        };
    } catch {
        return { ok: false, message: 'Could not reach the panel server to work out the dependencies.' };
    }
}

export async function uninstallMod(serverId: number, modId: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/mods/${modId}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function setModUpdatesPaused(serverId: number, modId: number, paused: boolean): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/mods/${modId}/updates-paused`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ paused }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Helper: pick the "primary" file from a Modrinth version. If there's only
// one file it's the obvious one; otherwise prefer the file marked primary.
export function pickPrimaryFile(v: ModrinthVersion): ModrinthVersionFile | null {
    if (!v.files || v.files.length === 0) return null;
    return v.files.find(f => f.primary) || v.files[0];
}

// --- Modpack contents snapshot (Content-tab cross-check) ---

export interface ServerModpackContent {
    id: number;
    serverId: number;
    subServerName: string;
    modrinthProjectId: string;
    modrinthVersionId: string;
    modrinthVersionNumber: string;
    fileName: string;
    side: string;
}

// Fail-open: returns [] on any error. The cross-check is advisory, so the tab
// must keep working when the snapshot is unavailable.
export async function getServerModpackContents(serverId: number): Promise<ServerModpackContent[]> {
    return (await getServerModpackInfo(serverId)).contents;
}

/**
 * The snapshot plus whether the server runs a modpack at all. A Technic or
 * uploaded pack can be a modpack with no Modrinth-known members. A Core that
 * predates isModpack answers without it, and then the contents decide.
 * Fail-open, like the snapshot itself.
 */
export async function getServerModpackInfo(serverId: number): Promise<{ contents: ServerModpackContent[]; isModpack: boolean }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/modpack-contents`, { headers: getAuthHeader() });
        const data = await handleResponse(res);
        const contents: ServerModpackContent[] = (data as any).contents || [];
        const isModpack = typeof (data as any).isModpack === 'boolean' ? (data as any).isModpack : contents.length > 0;
        return { contents, isModpack };
    } catch (err) {
        handleError(err);
        return { contents: [], isModpack: false };
    }
}

// --- Rollback: what a mod USED to be -------------------------------------

/**
 * One superseded version of an installed mod. Core keeps the three most recent
 * per project, written when an install replaces a different version.
 *
 * fileName is the jar that version installed, and it is the field the whole
 * table exists for: server_mods is keyed on the project, so an update
 * overwrites it and nothing else in the system remembers what the previous
 * build was called.
 */
export interface ModHistoryEntry {
    id: number;
    modrinthProjectId: string;
    modrinthVersionId: string;
    title: string;
    fileName: string;
    targetDir: string;
    sha512: string;
    installedAt: string;
    replacedAt: string;
}

export async function getModHistory(serverId: number): Promise<ModHistoryEntry[]> {
    const res = await fetch(`${API_URL}/servers/${serverId}/mods/history`, { headers: getAuthHeader() });
    const data = await handleResponse(res);
    if (!(data as any)?.success) {
        throw new Error((data as any)?.message || 'Could not load the mod history.');
    }
    return (data as any).history || [];
}

/**
 * One Modrinth version by id, through Core's cached proxy.
 *
 * Rolling back needs the download URL and the hash of a build that is no longer
 * in any list the tab is holding - the version list is filtered by the server's
 * loader and Minecraft version, and the build being rolled back to may not
 * match today's filter at all. Asking for it by id sidesteps that entirely.
 *
 * Raises rather than returning null: the caller is about to replace a working
 * jar, and "no version" would read as "roll back to nothing".
 */
export async function getModrinthVersion(versionId: string): Promise<ModrinthVersion> {
    const res = await fetch(`${API_URL}/modrinth/version/${encodeURIComponent(versionId)}`, {
        headers: getAuthHeader(),
    });
    const data = await handleResponse(res);
    if (!data || (data as any).success === false || !(data as any).id) {
        throw new Error((data as any)?.message || 'Could not load that build from Modrinth.');
    }
    return data as unknown as ModrinthVersion;
}
