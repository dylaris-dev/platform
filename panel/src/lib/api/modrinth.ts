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
    if (params.limit) u.set('limit', String(params.limit));
    if (params.offset) u.set('offset', String(params.offset));
    if (params.index) u.set('index', params.index);
    return u.toString();
}

export async function searchModrinth(params: SearchParams): Promise<ModrinthSearchResult | null> {
    try {
        const res = await fetch(`${API_URL}/modrinth/search?${buildSearchURL(params)}`, {
            headers: getAuthHeader(),
        });
        if (!res.ok) return null;
        return res.json();
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
        const res = await fetch(url, { headers: getAuthHeader() });
        if (!res.ok) return [];
        return res.json();
    } catch { return []; }
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

export async function uninstallMod(serverId: number, modId: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/mods/${modId}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
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
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/modpack-contents`, { headers: getAuthHeader() });
        const data = await handleResponse(res);
        return (data as any).contents || [];
    } catch (err) {
        handleError(err);
        return [];
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
