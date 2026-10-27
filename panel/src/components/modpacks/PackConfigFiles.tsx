'use client';

import { useMemo, useState } from 'react';
import { FileBrowser, type FileBrowserAdapter } from '@dylaris/ui-filebrowser';
import { uploadContent, removeContent, type BuildContentEntry } from '@/lib/api/packs';
import { getContentText, setContentText } from '@/lib/api/packsPublish';
import { API_URL, getAuthHeader } from '@/lib/api/core';
import { toast } from '@/components/ui/Toast';

const clean = (path: string) => path.replace(/^\/+|\/+$/g, '');
const within = (path: string, base: string) => path === base || path.startsWith(base + '/');

export default function PackConfigFiles({
    packId,
    buildId,
    canEdit,
    canDelete,
    onChanged,
}: {
    packId: number;
    buildId: number;
    canEdit: boolean;
    canDelete: boolean;
    onChanged: () => Promise<void>;
}) {
    const [side, setSide] = useState<'both' | 'client' | 'server'>('both');
    const adapter = useMemo<FileBrowserAdapter>(() => {
        const base = `/packs/${packId}/builds/${buildId}`;
        const entries = async () => {
            const response = await fetch(`${API_URL}${base}/content`, { headers: getAuthHeader() });
            const result = await response.json();
            if (!response.ok || result.success === false) throw new Error(result.message || 'Could not read files.');
            return (result.content as BuildContentEntry[]).filter((e) => e.contentType === 'config' && e.side === side);
        };
        const folders = async () => {
            const r = await fetch(`${API_URL}${base}/folders`, { headers: getAuthHeader() });
            const data = await r.json();
            if (!r.ok) throw new Error(data.message || 'Could not load folders.');
            return data.folders as string[];
        };
        const folder = async (path: string, remove = false) => {
            if (remove ? !canDelete : !canEdit) throw new Error(`${remove ? 'Delete' : 'Edit'} access is required.`);
            const r = await fetch(`${API_URL}${base}/folders`, {
                method: remove ? 'DELETE' : 'POST',
                headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
                body: JSON.stringify({ path: clean(path) }),
            });
            const data = await r.json();
            if (!r.ok) throw new Error(data.message || 'Folder operation failed.');
            return { success: true };
        };
        const find = async (path: string) => {
            const entry = (await entries()).find((e) => e.targetPath === clean(path));
            if (!entry) throw new Error('File not found in this environment.');
            return entry;
        };
        const upload = async (path: string, file: File) => {
            if (!canEdit) throw new Error('Edit access is required.');
            const result = await uploadContent(packId, buildId, file, side, 'config', { targetPath: clean(path) });
            if (!result.success) throw new Error(result.message || 'Upload failed.');
        };
        const copy = async (source: string, target: string) => {
            const e = await find(source);
            const data = await getContentText(packId, buildId, e.id);
            if (!data.success) throw new Error(data.message || 'Only editable text files can be copied here.');
            await upload(target, new File([data.text], clean(target).split('/').pop() || 'config.txt'));
        };
        const copyFolders = async (source: string, target: string) => {
            const dirs = (await folders()).filter((d) => within(d, clean(source)));
            for (const d of dirs) await folder(clean(target) + d.slice(clean(source).length));
        };
        const remove = async (path: string) => {
            if (!canDelete) throw new Error('Delete access is required.');
            const all = await entries();
            const selected = all.filter((e) => within(e.targetPath, clean(path)));
            for (const e of selected) {
                const result = await removeContent(packId, buildId, e.id);
                if (!result.success) throw new Error(result.message || 'Some files could not be removed.');
            }
            const dirs = (await folders()).filter((d) => within(d, clean(path))).sort((a, b) => b.length - a.length);
            for (const d of dirs) await folder(d, true);
        };
        const attempt = async (fn: () => Promise<void>) => {
            try {
                await fn();
                await onChanged();
                return { success: true };
            } catch (e) {
                return { success: false, message: e instanceof Error ? e.message : 'File operation failed.' };
            }
        };
        const archive = async (path: string, selected: string[], all: boolean) => {
            const JSZip = (await import('jszip')).default;
            const zip = new JSZip();
            const prefix = clean(path);
            const rows = (await entries()).filter(
                (e) =>
                    (!prefix || e.targetPath.startsWith(prefix + '/')) &&
                    (all || selected.some((s) => within(e.targetPath, clean(prefix + '/' + s)))),
            );
            for (const e of rows) {
                const data = await getContentText(packId, buildId, e.id);
                if (!data.success)
                    throw new Error(
                        'A binary/bundled file cannot be downloaded here. Use the complete build download.',
                    );
                zip.file(e.targetPath, data.text);
            }
            saveBlob(await zip.generateAsync({ type: 'blob' }), 'modpack-configs.zip');
        };
        return {
            getFiles: async (path) => {
                try {
                    const prefix = clean(path);
                    const all = await entries();
                    const dirs = await folders();
                    const names = new Map<string, { name: string; is_dir: boolean; size: number }>();
                    for (const p of [...all.map((e) => e.targetPath), ...dirs.map((d) => d + '/')]) {
                        if (prefix && !p.startsWith(prefix + '/')) continue;
                        const rest = prefix ? p.slice(prefix.length + 1) : p;
                        const name = rest.split('/')[0];
                        if (!name) continue;
                        const dir = rest.includes('/');
                        names.set(name, { name, is_dir: dir, size: 0 });
                    }
                    return { success: true, files: [...names.values()] };
                } catch (e) {
                    return {
                        success: false,
                        files: [],
                        message: e instanceof Error ? e.message : 'Could not load files.',
                    };
                }
            },
            getFileContent: async (path) => {
                try {
                    const e = await find(path);
                    const data = await getContentText(packId, buildId, e.id);
                    return {
                        success: !!data.success,
                        content: data.text || '',
                        message: data.message,
                        readonly: !canEdit,
                    };
                } catch (e) {
                    return {
                        success: false,
                        content: '',
                        message: e instanceof Error ? e.message : 'Could not open file.',
                    };
                }
            },
            saveFile: (path, text) =>
                attempt(async () => {
                    if (!canEdit) throw new Error('Edit access is required.');
                    const e = await find(path);
                    const result = await setContentText(packId, buildId, e.id, text);
                    if (!result.success) throw new Error(result.message || 'Save failed.');
                }),
            createFile: (path, isDir) =>
                attempt(async () => {
                    if (isDir) await folder(path);
                    else await upload(path, new File([''], clean(path).split('/').pop() || 'config.txt'));
                }),
            deleteFile: (path) => attempt(() => remove(path)),
            renameFile: (source, target) =>
                attempt(async () => {
                    if (!canEdit || !canDelete) throw new Error('Renaming requires edit and delete access.');
                    if (!clean(source) || within(clean(target), clean(source)))
                        throw new Error('Choose a different destination outside the source folder.');
                    const rows = (await entries()).filter((e) => within(e.targetPath, clean(source)));
                    if (!rows.length) await folder(target);
                    for (const e of rows)
                        await copy(e.targetPath, clean(target) + e.targetPath.slice(clean(source).length));
                    await copyFolders(source, target);
                    await remove(source);
                }),
            copyFile: (source, target) =>
                attempt(async () => {
                    if (!clean(source) || within(clean(target), clean(source)))
                        throw new Error('Choose a different destination outside the source folder.');
                    const rows = (await entries()).filter((e) => within(e.targetPath, clean(source)));
                    if (!rows.length) await folder(target);
                    for (const e of rows)
                        await copy(e.targetPath, clean(target) + e.targetPath.slice(clean(source).length));
                    await copyFolders(source, target);
                }),
            uploadFiles: (path, files, onProgress) =>
                attempt(async () => {
                    let done = 0;
                    for (const file of Array.from(files)) {
                        if (file.name.toLowerCase().endsWith('.zip')) {
                            const JSZip = (await import('jszip')).default;
                            const zip = await JSZip.loadAsync(file);
                            const zipped = Object.values(zip.files);
                            if (zipped.length > 1000) throw new Error('Too many archive entries (maximum 1000).');
                            let total = 0;
                            for (const item of zipped) {
                                if (item.dir) {
                                    await folder(clean(path + '/' + item.name));
                                    continue;
                                }
                                const size = (item as unknown as { _data?: { uncompressedSize?: number } })._data
                                    ?.uncompressedSize;
                                if (size === undefined || size > 8 * 1024 * 1024 || (total += size) > 64 * 1024 * 1024)
                                    throw new Error('Config archive exceeds the 8 MB/file or 64 MB total limit.');
                                const data = await item.async('uint8array');
                                if (data.byteLength !== size)
                                    throw new Error('Config archive contains an invalid file size.');
                                await upload(
                                    clean(path + '/' + item.name),
                                    new File([data as Uint8Array<ArrayBuffer>], item.name.split('/').pop() || 'file'),
                                );
                            }
                        } else await upload(clean(path + '/' + (file.webkitRelativePath || file.name)), file);
                        onProgress((++done / files.length) * 100);
                    }
                }),
            downloadFile: async (path, _id, isDir) => {
                try {
                    if (isDir) {
                        await archive(path, [], true);
                        return;
                    }
                    const e = await find(path);
                    const result = await getContentText(packId, buildId, e.id);
                    if (!result.success)
                        throw new Error('Use the complete build download for binary files or legacy bundles.');
                    saveBlob(
                        new Blob([result.text], { type: 'text/plain;charset=utf-8' }),
                        clean(path).split('/').pop() || 'config.txt',
                    );
                } catch (e) {
                    toast(e instanceof Error ? e.message : 'Download failed.', false);
                    throw e;
                }
            },
            selectiveDownload: async (path, selected, all) => archive(path, selected, all),
            getUserLimits: async () => ({
                success: true,
                uploadLimit: 64 * 1024 * 1024,
                downloadLimit: 64 * 1024 * 1024,
            }),
        };
    }, [packId, buildId, side, canEdit, canDelete, onChanged]);
    return (
        <div className="flex flex-col min-h-0 flex-1 gap-3">
            <div role="group" aria-label="Configuration environment" className="flex flex-wrap gap-2">
                {(['both', 'client', 'server'] as const).map((value) => (
                    <button
                        key={value}
                        aria-pressed={side === value}
                        onClick={() => setSide(value)}
                        className={`btn btn-sm ${side === value ? 'btn-primary' : 'btn-secondary'}`}
                    >
                        {value === 'both'
                            ? 'Shared files'
                            : value === 'client'
                              ? 'Client-only files'
                              : 'Server-only files'}
                    </button>
                ))}
            </div>
            <p className="text-xs text-(--base-06)">
                Paths are relative to the Minecraft instance. Text editing is limited to 1 MB. Binary files and legacy
                ZIP bundles are available through the complete build download. Empty folders are workspace metadata;
                adding a file includes the folder in exports.
            </p>
            <div className="flex-1 min-h-0 overflow-auto">
                <FileBrowser
                    key={side}
                    currentServerPath=""
                    rootLabel="Build files"
                    allowReadOnlyDownloads
                    adapter={adapter}
                    readOnly={!canEdit && !canDelete}
                    notify={toast}
                />
            </div>
        </div>
    );
}

function saveBlob(blob: Blob, name: string) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
}
