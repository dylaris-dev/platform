// Panel API client for /api/templates: user-owned starting points for a new
// server, their files and their shares.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export type TemplateKind = 'server' | 'proxy';
export type TemplateVisibility = 'personal' | 'admin' | 'platform';
export type ShareLevel = 'use' | 'edit';

export interface Template {
    id: number;
    ownerId: string;
    ownerName: string;
    name: string;
    kind: TemplateKind;
    visibility: TemplateVisibility;
    software: string;
    mcVersion: string;
    softwareVersion: string;
    storageConnectionId: number;
    /** The owner's own storage the files live in; absent means Dylaris storage. */
    backupStorageId?: number | null;
    sizeBytes: number;
    createdAt: string;
    updatedAt: string;
    canEdit: boolean;
    canShare: boolean;
    /** The caller's share on someone else's template: "use", "edit" or "". */
    shareLevel: ShareLevel | '';
    /** Set while the owner is suspended: what a write would be refused with. Reads, download and delete still work. */
    writeRefusal?: string;
    /** This template's size cap: null on the owner's own storage (no limit). Older Cores omit it. */
    maxSizeBytes?: number | null;
}

export interface TemplateMeta {
    name: string;
    kind: TemplateKind;
    visibility?: TemplateVisibility;
    software?: string;
    mcVersion?: string;
    softwareVersion?: string;
}

export interface TemplateFile {
    name: string;
    is_dir: boolean;
    size: number;
    /** Set when the entry is a Modrinth reference the node downloads on install, not a stored file. */
    modrinth?: { id: number; projectId: string; versionId: string };
}

export interface TemplateShare {
    templateId: number;
    userId: string;
    username: string;
    level: ShareLevel;
    createdAt: string;
}

/** A file Core left out of a template, with why. */
export interface SkippedFile {
    path: string;
    reason: string;
}

type Result<T = object> = { success: boolean; status?: number; message?: string } & Partial<T>;

async function call<T>(path: string, init: RequestInit = {}): Promise<Result<T>> {
    try {
        const res = await fetch(`${API_URL}${path}`, { ...init, headers: { ...getAuthHeader(), ...(init.headers ?? {}) } });
        return (await handleResponse(res)) as Result<T>;
    } catch (err) {
        return handleError(err) as Result<T>;
    }
}

const json = (method: string, body: unknown): RequestInit => ({
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
});

export const listTemplates = () =>
    call<{ templates: Template[]; maxSizeBytes: number | null; maxPerUser: number | null }>(`/templates`);

export const getTemplate = (id: number) => call<Template>(`/templates/${id}`);

/** A plain same-origin link: the browser sends the session cookie, Core streams a zip. */
export const templateDownloadUrl = (id: number) => `${API_URL}/templates/${id}/download`;

export const createTemplate = (meta: TemplateMeta) => call<Template>(`/templates`, json('POST', meta));

// ponytail: plain fetch, no upload progress; switch to XHR like files.ts if large zips feel stuck.
export function uploadTemplate(meta: TemplateMeta, file: File) {
    const form = new FormData();
    form.append('meta', JSON.stringify(meta));
    form.append('file', file);
    return call<{ template: Template; skipped: SkippedFile[] }>(`/templates/upload`, { method: 'POST', body: form });
}

export const captureTemplate = (req: TemplateMeta & { serverId: number; subServer: string; selected: string[] }) =>
    call<{ template: Template; skipped: SkippedFile[]; warning?: string }>(`/templates/capture`, json('POST', req));

export const updateTemplate = (id: number, patch: Partial<Omit<TemplateMeta, 'kind'>>) =>
    call<Template>(`/templates/${id}`, json('PATCH', patch));

/** warning: the row went but files may remain in the owner's own storage. */
export const deleteTemplate = (id: number) => call<{ warning: string }>(`/templates/${id}`, { method: 'DELETE' });

export const listTemplateFiles = (id: number) => call<{ files: TemplateFile[] }>(`/templates/${id}/files`);

/** One bundled request: entries[i].path is where file i is stored (relative, incl. sub-folders). */
export function addTemplateFiles(id: number, entries: { path: string; file: File }[]) {
    const form = new FormData();
    for (const e of entries) {
        form.append('path', e.path);
        form.append('file', e.file);
    }
    return call<{ sizeBytes: number; skipped: SkippedFile[] }>(`/templates/${id}/files`, { method: 'POST', body: form });
}

export const deleteTemplateFile = (id: number, path: string) =>
    call<{ sizeBytes: number }>(`/templates/${id}/files?path=${encodeURIComponent(path)}`, { method: 'DELETE' });

export const getTemplateText = (id: number, path: string) =>
    call<{ path: string; content: string }>(`/templates/${id}/files/text?path=${encodeURIComponent(path)}`);

export const putTemplateText = (id: number, path: string, content: string) =>
    call<{ sizeBytes: number }>(`/templates/${id}/files/text`, json('PUT', { path, content }));

export const listTemplateShares = (id: number) => call<{ shares: TemplateShare[] }>(`/templates/${id}/shares`);

export const putTemplateShare = (id: number, username: string, level: ShareLevel) =>
    call<{ userId: string; username: string; level: ShareLevel }>(`/templates/${id}/shares`, json('PUT', { username, level }));

export const deleteTemplateShare = (id: number, userId: string) =>
    call(`/templates/${id}/shares/${encodeURIComponent(userId)}`, { method: 'DELETE' });

export const listOwnerShares = () => call<{ shares: TemplateShare[] }>(`/templates/owner-shares`);

export const putOwnerShare = (username: string, level: ShareLevel) =>
    call<{ userId: string; username: string; level: ShareLevel }>(`/templates/owner-shares`, json('PUT', { username, level }));

export const deleteOwnerShare = (userId: string) =>
    call(`/templates/owner-shares?userId=${encodeURIComponent(userId)}`, { method: 'DELETE' });

export interface TemplateModrinthRef {
    id: number;
    templateId: number;
    path: string;
    projectId: string;
    versionId: string;
    fileName: string;
    sizeBytes: number;
}

/** Keeps a Modrinth version's primary file as a reference; warning: Modrinth does not list it for this software or Minecraft version. */
export const addTemplateModrinth = (id: number, versionId: string) =>
    call<{ reference: TemplateModrinthRef; warning?: string; sizeBytes: number }>(`/templates/${id}/modrinth`, json('POST', { versionId }));

export const deleteTemplateModrinth = (id: number, refId: number) =>
    call<{ sizeBytes: number }>(`/templates/${id}/modrinth/${refId}`, { method: 'DELETE' });
