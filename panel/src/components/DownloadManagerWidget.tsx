"use client";

import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { Download, X, CircleCheck, CircleAlert, Trash2, Loader2, FolderOpen } from 'lucide-react';
import { toast } from '@/components/ui/Toast';
import { ProgressRing } from '@/components/UploadManagerWidget';
import { formatBytes } from '@/lib/format';
import { getWailsApp } from '@/lib/adapters';
import {
    createDownloadStore,
    downloadPercent,
    downloadSpeed,
    downloadEta,
    aggregatePercent,
    type DownloadEntry,
} from '@/lib/downloadManager';

// The one download list of the panel. The authed layout hands it to the file
// browser through DownloadTrackerContext, so downloads outlive the page that
// started them.
export const downloadStore = createDownloadStore({ notify: (message, ok) => toast(message, ok) });

const noEntries: DownloadEntry[] = [];

function useDownloads(): DownloadEntry[] {
    return useSyncExternalStore(downloadStore.subscribe, downloadStore.getSnapshot, () => noEntries);
}

function formatEta(seconds: number): string {
    if (seconds < 60) return `${seconds}s left`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s left`;
    return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m left`;
}

// Navbar entry for downloads, built like the upload and mod install widgets
// beside it. Hidden while there is nothing to show.
export default function DownloadManagerWidget() {
    const entries = useDownloads();
    const [open, setOpen] = useState(false);
    const wrapRef = useRef<HTMLDivElement>(null);
    const buttonRef = useRef<HTMLButtonElement>(null);

    const active = entries.filter(e => e.state === 'running').length;
    const failed = entries.some(e => e.state === 'failed');

    // Open the list when the first download starts: the file browser no
    // longer shows a card of its own, so this is where it went.
    const [prevActive, setPrevActive] = useState(0);
    if (active !== prevActive) {
        setPrevActive(active);
        if (prevActive === 0 && active > 0) setOpen(true);
    }

    useEffect(() => {
        if (!open) return;
        const onClick = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
        };
        const onKey = (e: KeyboardEvent) => {
            if (e.key !== 'Escape') return;
            setOpen(false);
            buttonRef.current?.focus();
        };
        document.addEventListener('click', onClick);
        document.addEventListener('keydown', onKey);
        return () => {
            document.removeEventListener('click', onClick);
            document.removeEventListener('keydown', onKey);
        };
    }, [open]);

    if (entries.length === 0) return null;

    const pct = aggregatePercent(entries);
    const label = active > 0
        ? `${active} download${active !== 1 ? 's' : ''} running${pct !== null ? `, ${pct}%` : ''}`
        : failed ? 'Downloads finished, some failed' : 'Downloads finished';
    const tone = active > 0
        ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border)'
        : failed
            ? 'bg-(--warning-ghost) text-(--warning-light) border-(--warning)/30'
            : 'bg-(--success)/10 text-(--success-light) border-(--success)/20';

    return (
        <div ref={wrapRef} className="relative mr-2">
            <button
                ref={buttonRef}
                type="button"
                onClick={() => setOpen(o => !o)}
                title={label}
                aria-label={label}
                aria-expanded={open}
                aria-controls="download-manager-list"
                className={`relative flex items-center justify-center w-9 h-9 rounded-md transition-colors border hover:brightness-110 active:brightness-95 ${
                    open ? 'bg-(--base-03) border-(--base-04) text-(--base-09)' : tone
                }`}
            >
                {active === 0 ? (
                    <Download size={16} />
                ) : pct !== null ? (
                    <ProgressRing progress={pct / 100}><Download size={12} /></ProgressRing>
                ) : (
                    <Loader2 size={16} className="animate-spin motion-reduce:animate-none" />
                )}
                {active > 0 && (
                    <span className="absolute -top-1 -right-1 min-w-4 h-4 px-1 rounded-full bg-(--accent-light) text-(--base-00) text-[10px] font-mono font-bold flex items-center justify-center leading-none">
                        {active}
                    </span>
                )}
            </button>

            {open && (
                <div
                    id="download-manager-list"
                    className="dropdown-menu right-0 mt-3 w-96 animate-fade-in origin-top-right"
                    role="region"
                    aria-label="Downloads"
                >
                    <div className="flex items-center justify-between px-4 py-2 border-b border-(--base-03)">
                        <div className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                            Downloads ({entries.length})
                        </div>
                        <div className="flex items-center gap-1">
                            {entries.some(e => e.state !== 'running') && (
                                <button
                                    type="button"
                                    onClick={downloadStore.clearFinished}
                                    className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) p-1 rounded"
                                    title="Clear finished downloads"
                                    aria-label="Clear finished downloads"
                                >
                                    <Trash2 size={11} />
                                </button>
                            )}
                            <button
                                type="button"
                                onClick={() => { setOpen(false); buttonRef.current?.focus(); }}
                                className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) p-1 rounded"
                                aria-label="Close"
                            >
                                <X size={12} />
                            </button>
                        </div>
                    </div>
                    <DownloadList entries={entries} />
                </div>
            )}
        </div>
    );
}

function DownloadList({ entries }: { entries: DownloadEntry[] }) {
    // Speed and ETA age even when no event arrives (a stalled transfer), so
    // the open list re-reads the clock every second.
    const [now, setNow] = useState(() => Date.now());
    useEffect(() => {
        const t = setInterval(() => setNow(Date.now()), 1000);
        return () => clearInterval(t);
    }, []);
    const lastDone = downloadStore.lastDoneId();
    const canReveal = typeof getWailsApp()?.RevealInExplorer === 'function';
    return (
        <ul className="max-h-[60vh] overflow-y-auto">
            {[...entries].reverse().map(e => (
                <DownloadRow key={e.id} entry={e} now={now} revealable={canReveal && e.id === lastDone} />
            ))}
        </ul>
    );
}

async function reveal() {
    try {
        await getWailsApp()?.RevealInExplorer?.();
    } catch (err) {
        toast(err instanceof Error ? err.message : 'Could not open the folder.', false);
    }
}

function DownloadRow({ entry: e, now, revealable }: { entry: DownloadEntry; now: number; revealable: boolean }) {
    const pct = downloadPercent(e);
    const running = e.state === 'running';
    const speed = downloadSpeed(e, now);
    const eta = downloadEta(e, now);
    const barColor = e.state === 'failed'
        ? 'bg-(--error-light)'
        : e.state === 'done' ? 'bg-(--success-light)' : 'bg-(--accent-light)';

    const sizes = e.total > 0 && running
        ? `${formatBytes(e.loaded)} / ${formatBytes(e.total)}`
        : formatBytes(e.loaded);

    return (
        <li className="px-3 py-2.5 border-b border-(--base-03) last:border-b-0">
            <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1 text-sm text-(--base-09) truncate" title={e.savePath ?? e.filename}>
                    {e.filename}
                </div>
                <div className="shrink-0 flex items-center gap-1">
                    {e.state === 'done' && <CircleCheck size={14} className="text-(--success-light)" aria-hidden />}
                    {e.state === 'failed' && <CircleAlert size={14} className="text-(--error-light)" aria-hidden />}
                    {revealable && (
                        <button
                            type="button"
                            onClick={reveal}
                            className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) p-1 rounded"
                            title="Show in folder"
                            aria-label={`Show ${e.filename} in folder`}
                        >
                            <FolderOpen size={12} />
                        </button>
                    )}
                    {!running && (
                        <button
                            type="button"
                            onClick={() => downloadStore.remove(e.id)}
                            className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) p-1 rounded"
                            title="Dismiss"
                            aria-label={`Dismiss ${e.filename}`}
                        >
                            <X size={12} />
                        </button>
                    )}
                </div>
            </div>

            <div
                className="mt-1.5 h-1 rounded-full bg-(--base-03) overflow-hidden"
                role="progressbar"
                aria-label={`Download of ${e.filename}`}
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={pct ?? undefined}
                aria-busy={running}
            >
                {pct === null && running ? (
                    <div className={`h-full rounded-full ${barColor} progress-indeterminate`} />
                ) : (
                    <div
                        className={`h-full rounded-full ${barColor} transition-[width] duration-200 ease-out motion-reduce:transition-none`}
                        style={{ width: `${e.state === 'failed' && pct === null ? 100 : pct}%` }}
                    />
                )}
            </div>

            <div className="mt-1 flex items-center justify-between gap-2 font-mono text-[10px] text-(--base-06) tabular-nums">
                <span className="shrink-0">
                    {pct !== null && running ? `${pct}% · ${sizes}` : sizes}
                </span>
                <span className={`truncate text-right ${e.state === 'failed' ? 'text-(--error-light)' : ''}`} title={e.error}>
                    {running
                        ? speed > 0
                            ? `${formatBytes(speed)}/s${eta !== null ? ` · ${formatEta(eta)}` : ''}`
                            : e.reported ? 'Waiting for data' : 'Starting'
                        : e.state === 'done'
                            ? 'Completed'
                            : `Failed: ${e.error}`}
                </span>
            </div>
        </li>
    );
}
