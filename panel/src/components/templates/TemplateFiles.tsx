'use client';

import { useMemo, useState } from 'react';
import { Loader2, Package, Plus, Trash2 } from 'lucide-react';
import { FileBrowser, type FileBrowserAdapter } from '@dylaris/ui-filebrowser';
import {
    addTemplateFiles, deleteTemplateFile, deleteTemplateModrinth, getTemplateText, listTemplateFiles, putTemplateText,
    type SkippedFile, type TemplateFile,
} from '@/lib/api/templates';
import { bundleFiles, retryableFailure } from '@/lib/templateBundle';
import { templateFolderEntries, templateModrinthTarget } from '@/lib/templates';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import TemplateModrinthDialog from './TemplateModrinthDialog';

const clean = (path: string) => path.replace(/^\/+|\/+$/g, '');
const within = (path: string, base: string) => path === base || path.startsWith(base + '/');

// The template's files over FileBrowser. Template storage is a flat set of
// paths, so a folder exists only while it holds a file, and Core only reads
// back text: copy, rename and download work on text files.
export default function TemplateFiles({
    templateId,
    software,
    mcVersion,
    canEdit,
    maxSizeBytes,
    onChanged,
    onSkipped,
}: {
    templateId: number;
    /** The saved software and Minecraft version: they decide the Modrinth filter and folder. */
    software: string;
    mcVersion: string;
    canEdit: boolean;
    maxSizeBytes: number | null;
    /** After any write, with the template's new size when Core sent one. */
    onChanged: (sizeBytes?: number) => void;
    /** Files Core refused on upload, shown by the page as an alert. */
    onSkipped: (skipped: SkippedFile[]) => void;
}) {
    const target = templateModrinthTarget(software);
    // Modrinth references ride in the file list; every listing refreshes them.
    const [refs, setRefs] = useState<TemplateFile[]>([]);
    // Remounts the browser after a change made outside it, so it lists again.
    const [rev, setRev] = useState(0);
    const [adding, setAdding] = useState(false);
    const [removing, setRemoving] = useState<number | null>(null);
    const adapter = useMemo<FileBrowserAdapter>(() => {
        const files = async () => {
            const res = await listTemplateFiles(templateId);
            if (!res.success) throw new Error(res.message || 'Could not load the files.');
            const all = res.files ?? [];
            setRefs(all.filter((f) => f.modrinth));
            return all;
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
            const ref = rows.find((f) => f.modrinth);
            if (ref) throw new Error(`${ref.name} is a Modrinth reference and cannot be copied or moved. Remove it and add it again from Modrinth instead.`);
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
                    return { success: true, files: templateFolderEntries(await files(), clean(path)) };
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
                    // Files go up in bundles (Core caps a request at 200 files / 100 MiB),
                    // one after the other. A failed bundle is retried once.
                    const entries = Array.from(list).map((file) => ({
                        file,
                        size: file.size,
                        path: clean([clean(path), clean(file.webkitRelativePath || file.name)].filter(Boolean).join('/')),
                    }));
                    const skipped: SkippedFile[] = [];
                    const failed: string[] = [];
                    let reason = '';
                    let done = 0;
                    try {
                        for (const batch of bundleFiles(entries)) {
                            let res;
                            for (let tryNo = 0; tryNo < 2; tryNo++) {
                                try {
                                    res = await addTemplateFiles(templateId, batch);
                                } catch {
                                    res = undefined;
                                }
                                if (!retryableFailure(res)) break;
                            }
                            if (res?.success) {
                                skipped.push(...(res.skipped ?? []));
                                onChanged(res.sizeBytes);
                            } else {
                                failed.push(...batch.map((b) => b.path));
                                reason ||= res?.message || '';
                            }
                            done += batch.length;
                            onProgress((done / entries.length) * 100);
                        }
                    } finally {
                        if (skipped.length) onSkipped(skipped);
                    }
                    if (failed.length) {
                        const shown = failed.slice(0, 5).join(', ');
                        throw new Error(`${failed.length} file(s) could not be uploaded${reason ? ` (${reason})` : ''}: ${shown}${failed.length > 5 ? ', ...' : ''}`);
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

    const changed = (sizeBytes?: number) => {
        onChanged(sizeBytes);
        setRev((n) => n + 1);
    };
    const removeRef = async (f: TemplateFile) => {
        if (!f.modrinth || removing !== null) return;
        setRemoving(f.modrinth.id);
        try {
            const res = await deleteTemplateModrinth(templateId, f.modrinth.id);
            if (!res.success) {
                toast(res.message || 'Could not remove the reference.', false);
                return;
            }
            toast(`Removed ${f.name}.`, true);
            changed(res.sizeBytes);
        } finally {
            setRemoving(null);
        }
    };
    const addButton = canEdit && (
        <button
            type="button"
            className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
            disabled={!target}
            title={target ? 'Search Modrinth and add a mod or plugin' : 'Set the template software to a loader such as paper or fabric first'}
            onClick={() => setAdding(true)}
        >
            <Plus size={12} /> Add from Modrinth
        </button>
    );

    return (
        <div className="flex flex-col min-h-0 flex-1 gap-3">
            <p className="text-xs text-(--base-06)">
                Paths are relative to the server folder. Text files up to 1 MB open in the editor; copy, move and
                download work on text files. Entries marked Modrinth are references the node downloads on install.
            </p>
            <div className="flex-1 min-h-0 overflow-auto">
                <FileBrowser
                    key={rev}
                    currentServerPath=""
                    rootLabel="Template files"
                    allowReadOnlyDownloads
                    adapter={adapter}
                    readOnly={!canEdit}
                    notify={toast}
                    toolbarActions={addButton || undefined}
                />
            </div>
            {refs.length > 0 && (
                <section aria-label="Modrinth references" className="space-y-2">
                    <h3 className="input-label">From Modrinth</h3>
                    <p className="text-xs text-(--base-06)">Not stored in the template: the node downloads these from Modrinth on install.</p>
                    <ul className="divide-y divide-(--base-03) border border-(--base-03) rounded-md">
                        {refs.map((f) => (
                            <li key={f.modrinth!.id} className="flex items-center gap-2 px-3 py-2 text-sm min-w-0">
                                <Badge variant="accent" icon={<Package size={10} />}>Modrinth</Badge>
                                <span className="font-mono text-xs truncate min-w-0 flex-1" title={f.name}>{f.name}</span>
                                <a
                                    href={`https://modrinth.com/project/${encodeURIComponent(f.modrinth!.projectId)}/version/${encodeURIComponent(f.modrinth!.versionId)}`}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    className="text-xs text-(--base-06) hover:text-(--base-09) focus-visible:text-(--base-09) shrink-0"
                                >
                                    View
                                </a>
                                {canEdit && (
                                    <button
                                        type="button"
                                        className="btn btn-ghost btn-sm disabled:opacity-40 disabled:cursor-not-allowed shrink-0"
                                        disabled={removing !== null}
                                        onClick={() => removeRef(f)}
                                        aria-label={`Remove ${f.name}`}
                                    >
                                        {removing === f.modrinth!.id ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                    </button>
                                )}
                            </li>
                        ))}
                    </ul>
                </section>
            )}
            {adding && target && (
                <TemplateModrinthDialog
                    templateId={templateId}
                    target={target}
                    mcVersion={mcVersion.trim()}
                    addedProjectIds={refs.map((f) => f.modrinth!.projectId)}
                    onAdded={changed}
                    onClose={() => setAdding(false)}
                />
            )}
        </div>
    );
}
