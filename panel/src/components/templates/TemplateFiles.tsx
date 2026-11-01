'use client';

import { useMemo } from 'react';
import { FileBrowser, type FileBrowserAdapter } from '@dylaris/ui-filebrowser';
import {
    addTemplateFiles, deleteTemplateFile, getTemplateText, listTemplateFiles, putTemplateText,
    type SkippedFile,
} from '@/lib/api/templates';
import { toast } from '@/components/ui/Toast';

const clean = (path: string) => path.replace(/^\/+|\/+$/g, '');
const within = (path: string, base: string) => path === base || path.startsWith(base + '/');
const dirOf = (path: string) => (path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '');

// The template's files over FileBrowser. Template storage is a flat set of
// paths, so a folder exists only while it holds a file, and Core only reads
// back text: copy, rename and download work on text files.
export default function TemplateFiles({
    templateId,
    canEdit,
    maxSizeBytes,
    onChanged,
    onSkipped,
}: {
    templateId: number;
    canEdit: boolean;
    maxSizeBytes: number | null;
    /** After any write, with the template's new size when Core sent one. */
    onChanged: (sizeBytes?: number) => void;
    /** Files Core refused on upload, shown by the page as an alert. */
    onSkipped: (skipped: SkippedFile[]) => void;
}) {
    const adapter = useMemo<FileBrowserAdapter>(() => {
        const files = async () => {
            const res = await listTemplateFiles(templateId);
            if (!res.success) throw new Error(res.message || 'Could not load the files.');
            return res.files ?? [];
        };
        const text = async (path: string) => {
            const res = await getTemplateText(templateId, clean(path));
            if (!res.success) throw new Error(res.message || 'Could not read the file.');
            return res.content ?? '';
        };
        const write = async (path: string, content: string) => {
            if (!canEdit) throw new Error('Edit access is required.');
            const res = await putTemplateText(templateId, clean(path), content);
            if (!res.success) throw new Error(res.message || 'Save failed.');
            onChanged(res.sizeBytes);
        };
        const remove = async (path: string) => {
            if (!canEdit) throw new Error('Edit access is required.');
            const res = await deleteTemplateFile(templateId, clean(path));
            if (!res.success) throw new Error(res.message || 'Delete failed.');
            onChanged(res.sizeBytes);
        };
        const copyTree = async (source: string, target: string) => {
            const src = clean(source);
            const dst = clean(target);
            if (!src || within(dst, src)) throw new Error('Choose a destination outside the source folder.');
            const rows = (await files()).filter((f) => within(f.name, src));
            if (!rows.length) throw new Error('Nothing to copy.');
            for (const f of rows) {
                let content: string;
                try {
                    content = await text(f.name);
                } catch (e) {
                    throw new Error(`${f.name}: ${e instanceof Error ? e.message : 'not a text file'}. Only text files can be copied or moved here.`);
                }
                await write(dst + f.name.slice(src.length), content);
            }
        };
        const attempt = async (fn: () => Promise<void>) => {
            try {
                await fn();
                return { success: true };
            } catch (e) {
                return { success: false, message: e instanceof Error ? e.message : 'File operation failed.' };
            }
        };
        const saveText = async (path: string) => {
            const blob = new Blob([await text(path)], { type: 'text/plain;charset=utf-8' });
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = clean(path).split('/').pop() || 'file.txt';
            a.click();
            setTimeout(() => URL.revokeObjectURL(url), 30000);
        };
        const noFolderDownload = () => {
            toast('Folders cannot be downloaded from a template. Open a text file to download it.', false);
        };
        return {
            getFiles: async (path) => {
                try {
                    const prefix = clean(path);
                    const names = new Map<string, { name: string; is_dir: boolean; size: number }>();
                    for (const f of await files()) {
                        if (prefix && !f.name.startsWith(prefix + '/')) continue;
                        const rest = prefix ? f.name.slice(prefix.length + 1) : f.name;
                        const name = rest.split('/')[0];
                        if (!name) continue;
                        const isDir = rest.includes('/');
                        const prev = names.get(name);
                        names.set(name, { name, is_dir: isDir, size: (prev?.size ?? 0) + f.size });
                    }
                    return { success: true, files: [...names.values()] };
                } catch (e) {
                    return { success: false, files: [], message: e instanceof Error ? e.message : 'Could not load the files.' };
                }
            },
            getFileContent: async (path) => {
                try {
                    return { success: true, content: await text(path), readonly: !canEdit };
                } catch (e) {
                    return { success: false, content: '', message: e instanceof Error ? e.message : 'Could not open the file.' };
                }
            },
            saveFile: (path, content) => attempt(() => write(path, content)),
            createFile: (path, isDir) =>
                attempt(async () => {
                    if (isDir) throw new Error('A template folder exists once it holds a file. Create or upload a file inside it instead.');
                    await write(path, '');
                }),
            deleteFile: (path) => attempt(() => remove(path)),
            renameFile: (source, target) =>
                attempt(async () => {
                    await copyTree(source, target);
                    await remove(source);
                }),
            copyFile: (source, target) => attempt(() => copyTree(source, target)),
            uploadFiles: (path, list, onProgress) =>
                attempt(async () => {
                    if (!canEdit) throw new Error('Edit access is required.');
                    // Core stores each part under one folder, so a folder upload
                    // goes up one request per subfolder.
                    const byDir = new Map<string, File[]>();
                    for (const f of Array.from(list)) {
                        const dir = clean([clean(path), dirOf(f.webkitRelativePath || '')].filter(Boolean).join('/'));
                        byDir.set(dir, [...(byDir.get(dir) ?? []), f]);
                    }
                    const skipped: SkippedFile[] = [];
                    let done = 0;
                    try {
                        for (const [dir, group] of byDir) {
                            const res = await addTemplateFiles(templateId, dir, group);
                            if (!res.success) throw new Error(res.message || 'Upload failed.');
                            skipped.push(...(res.skipped ?? []));
                            onChanged(res.sizeBytes);
                            done += group.length;
                            onProgress((done / list.length) * 100);
                        }
                    } finally {
                        if (skipped.length) onSkipped(skipped);
                    }
                }),
            downloadFile: async (path, _uuid, isDir) => {
                if (isDir) return noFolderDownload();
                try {
                    await saveText(path);
                } catch (e) {
                    toast(e instanceof Error ? e.message : 'Download failed.', false);
                    throw e;
                }
            },
            selectiveDownload: noFolderDownload,
            getUserLimits: async () => ({ success: true, uploadLimit: maxSizeBytes ?? 0, downloadLimit: 0 }),
        };
    }, [templateId, canEdit, maxSizeBytes, onChanged, onSkipped]);

    return (
        <div className="flex flex-col min-h-0 flex-1 gap-3">
            <p className="text-xs text-(--base-06)">
                Paths are relative to the server folder. Text files up to 1 MB open in the editor; copy, move and
                download work on text files.
            </p>
            <div className="flex-1 min-h-0 overflow-auto">
                <FileBrowser
                    currentServerPath=""
                    rootLabel="Template files"
                    allowReadOnlyDownloads
                    adapter={adapter}
                    readOnly={!canEdit}
                    notify={toast}
                />
            </div>
        </div>
    );
}
