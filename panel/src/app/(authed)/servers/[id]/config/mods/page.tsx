"use client";

import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FileCode2, RefreshCw, Search, Save, AlertTriangle } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { useRouteId } from '@/lib/routeParams';
import { getFiles, getFileContent, saveFile } from '@/lib/api';
import { findConfigFiles, type ListedEntry } from '@/lib/modConfigFiles';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import Tooltip from '@/components/ui/Tooltip';

const CodeMirrorEditor = lazy(() => import('@dylaris/ui-filebrowser').then(m => ({ default: m.CodeMirrorEditor })));

/**
 * Beta: the config files of the server's mods and plugins in one list, with
 * the file manager's editor beside it. It reads and writes through the same
 * file endpoints, so the same files.read / files.write permissions apply.
 */
export default function ModConfigsPage() {
    const serverId = Number(useRouteId('servers'));
    const { servers, featureFlags } = useAppData();
    const server = servers.find(s => s.id === serverId);
    const uuid = server?.uuid ?? '';
    const sub = server?.activeSubServer ?? '';

    const [files, setFiles] = useState<string[] | null>(null);
    const [truncated, setTruncated] = useState(false);
    const [listError, setListError] = useState<string | null>(null);
    const [query, setQuery] = useState('');
    const [open, setOpen] = useState<string | null>(null);
    const [text, setText] = useState('');
    const [saved, setSaved] = useState('');
    const [loadingFile, setLoadingFile] = useState(false);
    const [saving, setSaving] = useState(false);
    // Never while a file loads: until its text arrives the editor still holds
    // the previous file's, and a save would write that into the new path.
    const dirty = open !== null && !loadingFile && text !== saved;
    // Which load is current. A late answer for an earlier load is dropped, or a
    // slow load could land under the next file clicked - the same relative path
    // in another sub-server included - and a save would then write it there.
    const loadSeq = useRef(0);

    // Another sub-server is another tree: what is open belongs to the old one,
    // and a save would write it into the new one at the same relative path.
    // Reset during render (not in an effect) when the sub-server changes.
    const [openSub, setOpenSub] = useState(sub);
    if (openSub !== sub) {
        setOpenSub(sub);
        setOpen(null);
        setText('');
        setSaved('');
        setLoadingFile(false);
        loadSeq.current++;
    }

    // A scan of the sub-server switched away from must not fill the list.
    const scanSeq = useRef(0);
    const scan = useCallback(async () => {
        if (!uuid || !sub) return;
        const seq = ++scanSeq.current;
        setFiles(null);
        setListError(null);
        try {
            const res = await findConfigFiles(sub, async path => {
                const r = await getFiles(path, uuid);
                return r?.success === false || !Array.isArray(r?.files) ? null : (r.files as ListedEntry[]);
            });
            if (scanSeq.current !== seq) return;
            setFiles(res.files);
            setTruncated(res.truncated);
        } catch (e) {
            if (scanSeq.current !== seq) return;
            setListError(e instanceof Error ? e.message : 'Could not list the config files');
            setFiles([]);
        }
    }, [uuid, sub]);

    useEffect(() => { scan(); }, [scan]);

    const save = useCallback(async (): Promise<boolean> => {
        if (!open) return true;
        setSaving(true);
        try {
            const res = await saveFile(`${sub}/${open}`, text, uuid);
            if (res?.success === false) { toast(res.message || 'Save failed', false); return false; }
            setSaved(text);
            toast(`Saved ${open}. Most mods and plugins read it on the next restart.`);
            return true;
        } finally {
            setSaving(false);
        }
    }, [open, sub, text, uuid]);
    useUnsavedChanges({ dirty, save, discard: () => setText(saved), saving });

    const openFile = async (path: string) => {
        if (path === open) return;
        if (dirty && !await confirmDialog({
            title: 'Discard unsaved changes?',
            message: `${open} has changes that are not saved.`,
            confirmLabel: 'Discard',
        })) return;
        setOpen(path);
        setText('');
        setSaved('');
        setLoadingFile(true);
        const seq = ++loadSeq.current;
        try {
            const res = await getFileContent(`${sub}/${path}`, uuid);
            if (loadSeq.current !== seq) return;
            if (res?.success === false) {
                toast(res.message || 'Could not open the file', false);
                setOpen(null);
                return;
            }
            const content: string = res.content ?? res.text ?? '';
            setText(content);
            setSaved(content);
        } finally {
            if (loadSeq.current === seq) setLoadingFile(false);
        }
    };

    const shown = useMemo(() => {
        const q = query.trim().toLowerCase();
        return (files ?? []).filter(f => !q || f.toLowerCase().includes(q));
    }, [files, query]);

    if (!featureFlags?.modConfigs) {
        return <div className="srv-surface px-6 py-14 text-center text-sm text-(--base-06)">The config editor is not enabled on this platform.</div>;
    }
    if (!sub) {
        return <div className="srv-surface px-6 py-14 text-center text-sm text-(--base-06)">Install or select a server first; there are no config files yet.</div>;
    }

    return (
        <div className="flex flex-col lg:flex-row gap-6 h-[calc(100vh-16rem)] min-h-105">
            <aside className="srv-surface lg:w-80 shrink-0 flex flex-col overflow-hidden min-h-0">
                <div className="p-4 flex flex-col gap-3">
                    <div className="flex items-center justify-between gap-2">
                        <span className="srv-config-card-title flex items-center gap-2">
                            Mod and plugin configs
                            <span className="srv-config-pill">Beta</span>
                        </span>
                        <Tooltip content="Scan again">
                            <button type="button" onClick={scan} disabled={files === null} aria-label="Scan again" className="btn btn-ghost btn-icon btn-sm">
                                <RefreshCw size={14} className={files === null ? 'animate-spin motion-reduce:animate-none' : ''} />
                            </button>
                        </Tooltip>
                    </div>
                    <div className="relative">
                        <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06) pointer-events-none" />
                        <input
                            value={query}
                            onChange={e => setQuery(e.target.value)}
                            placeholder="Filter files"
                            aria-label="Filter config files"
                            className="input-field input-icon-left w-full"
                        />
                    </div>
                </div>
                <div className="flex-1 overflow-y-auto px-2 pb-2 min-h-0">
                    {files === null ? (
                        <div className="space-y-1.5 p-1.5" aria-busy="true">
                            {Array.from({ length: 8 }, (_, i) => <div key={i} className="h-6 rounded-md bg-(--base-03) animate-pulse motion-reduce:animate-none" />)}
                        </div>
                    ) : listError ? (
                        <p className="p-2 text-xs text-(--error-light)">{listError}</p>
                    ) : shown.length === 0 ? (
                        <p className="p-2 text-xs text-(--base-06)">
                            {files.length === 0 ? 'No config files in config/ or plugins/ yet. Most are written on the first start.' : 'Nothing matches the filter.'}
                        </p>
                    ) : (
                        <ul>
                            {shown.map(f => (
                                <li key={f}>
                                    <button
                                        onClick={() => openFile(f)}
                                        aria-current={f === open ? 'true' : undefined}
                                        className={`w-full text-left px-2.5 py-1.5 rounded-md font-mono text-xs truncate transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${
                                            f === open ? 'bg-(--base-03) text-(--accent-light)' : 'text-(--base-07) hover:bg-(--base-03) hover:text-(--base-09)'
                                        }`}
                                        title={f}
                                    >
                                        {f}
                                    </button>
                                </li>
                            ))}
                        </ul>
                    )}
                    {truncated && (
                        <p className="p-2 text-[11px] text-(--warning-light) flex items-start gap-1.5">
                            <AlertTriangle size={11} className="mt-0.5 shrink-0" />
                            Not every folder was scanned. Use the file manager for the rest.
                        </p>
                    )}
                </div>
            </aside>

            <section className="srv-surface flex-1 flex flex-col overflow-hidden min-h-0 min-w-0">
                {open === null ? (
                    <div className="flex-1 flex flex-col items-center justify-center gap-2 text-sm text-(--base-06) p-6 text-center">
                        <FileCode2 size={22} className="text-(--base-05)" />
                        Pick a file on the left to edit it.
                    </div>
                ) : (
                    <>
                        <div className="flex items-center justify-between gap-3 px-4 py-3 border-b border-(--base-03)">
                            <span className="font-mono text-xs text-(--base-08) truncate">{open}</span>
                            <div className="flex items-center gap-2 shrink-0">
                                {dirty && <span className="srv-config-pill" data-tone="warning">Unsaved</span>}
                                <button onClick={save} disabled={!dirty || saving} className="btn btn-primary btn-sm">
                                    <Save size={14} />
                                    {saving ? 'Saving…' : 'Save'}
                                </button>
                            </div>
                        </div>
                        <div className="flex-1 min-h-0">
                            {loadingFile ? (
                                <div className="h-full flex items-center justify-center text-sm text-(--base-07)">Loading…</div>
                            ) : (
                                <Suspense fallback={<div className="h-full flex items-center justify-center text-sm text-(--base-07)">Loading editor…</div>}>
                                    <CodeMirrorEditor value={text} onChange={setText} filename={open} className="h-full" />
                                </Suspense>
                            )}
                        </div>
                    </>
                )}
            </section>
        </div>
    );
}
