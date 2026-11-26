"use client";

import { useEffect, useState, useSyncExternalStore } from 'react';
import { Popover, PopoverContent, PopoverTrigger } from '@ui-sdk/react/popover';
import { Download, X, CircleCheck, CircleAlert, CircleStop, Trash2, Loader2, FolderOpen } from 'lucide-react';
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
export const downloadStore = createDownloadStore({
    notify: (message, ok) => toast(message, ok),
    cancel: savePath => { void getWailsApp()?.CancelDownload?.(savePath).catch(() => {}); },
});

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

    const active = entries.filter(e => e.state === 'running').length;
    const failed = entries.some(e => e.state === 'failed');

    // Open the list when the first download starts: the file browser no
    // longer shows a card of its own, so this is where it went.
    const [prevActive, setPrevActive] = useState(0);
    if (active !== prevActive) {
        setPrevActive(active);
        if (prevActive === 0 && active > 0) setOpen(true);
    }

    if (entries.length === 0) return null;

    const pct = aggregatePercent(entries);
    const label = active > 0
        ? `${active} download${active !== 1 ? 's' : ''} running${pct !== null ? `, ${pct}%` : ''}`
        : failed ? 'Downloads finished, some failed' : 'Downloads finished';
    const tone = active > 0
        ? 'text-(--accent-light)'
        : failed
            ? 'text-(--warning-light)'
            : 'text-(--success-light)';

    return (
        <div className="relative">
          <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger
                render={<button type="button" />}
                title={label}
                aria-label={label}
                className={`app-icon-btn relative ${open ? '' : tone}`}
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
            </PopoverTrigger>

            {/* initialFocus none: it also opens by itself when a download starts, and must not steal focus. */}
            <PopoverContent side="bottom" align="end" sideOffset={12} initialFocus="none" aria-label="Downloads" className="ui-popup w-96 max-w-[calc(100vw-1rem)]">
                <div>
                    <div className="flex items-center justify-between px-4 py-2.5">
                        <div className="text-sm font-medium text-(--base-09)">
                            Downloads ({entries.length})
                        </div>
                        <div className="flex items-center gap-1">
                            {entries.some(e => e.state !== 'running') && (
                                <button
                                    type="button"
                                    onClick={downloadStore.clearFinished}
                                    className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) hover:bg-(--base-02) active:bg-(--base-03) focus-visible:outline-2 focus-visible:outline-(--accent) p-1.5 rounded-md transition-colors"
                                    title="Clear finished downloads"
                                    aria-label="Clear finished downloads"
                                >
                                    <Trash2 size={16} />
                                </button>
                            )}
                            <button
                                type="button"
                                onClick={() => setOpen(false)}
                                className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) hover:bg-(--base-02) active:bg-(--base-03) focus-visible:outline-2 focus-visible:outline-(--accent) p-1.5 rounded-md transition-colors"
                                aria-label="Close"
                            >
                                <X size={16} />
                            </button>
                        </div>
                    </div>
                    <DownloadList entries={entries} />
                </div>
            </PopoverContent>
          </Popover>
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
    // An app older than 0.9.14 has no CancelDownload; its rows show no button.
    const canCancel = typeof getWailsApp()?.CancelDownload === 'function';
    return (
        <ul className="max-h-[60vh] overflow-y-auto">
            {[...entries].reverse().map(e => (
                <DownloadRow key={e.id} entry={e} now={now} revealable={canReveal && e.id === lastDone} cancellable={canCancel && downloadStore.canCancel(e)} />
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

function DownloadRow({ entry: e, now, revealable, cancellable }: { entry: DownloadEntry; now: number; revealable: boolean; cancellable: boolean }) {
    const pct = downloadPercent(e);
    const running = e.state === 'running';
    const speed = downloadSpeed(e, now);
    const eta = downloadEta(e, now);
    const barColor = e.state === 'failed'
        ? 'bg-(--error-light)'
        : e.state === 'done' ? 'bg-(--success-light)'
            : e.state === 'cancelled' ? 'bg-(--base-05)' : 'bg-(--accent-light)';

    const sizes = e.total > 0 && running
        ? `${formatBytes(e.loaded)} / ${formatBytes(e.total)}`
        : formatBytes(e.loaded);

    return (
        <li className="px-3 py-2.5 border-b border-(--base-03) last:border-b-0">
            <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1 text-sm text-(--base-09) truncate" title={e.savePath ?? e.filename}>
                    {e.filename}
                </div>
                <div className="shrink-0 flex items-center gap-1.5">
                    {e.state === 'done' && <CircleCheck size={16} className="text-(--success-light)" aria-hidden />}
                    {e.state === 'failed' && <CircleAlert size={16} className="text-(--error-light)" aria-hidden />}
                    {cancellable && (
                        <button
                            type="button"
                            onClick={() => downloadStore.cancel(e.id)}
                            className="text-(--base-06) hover:text-(--error-light) active:text-(--base-07) hover:bg-(--base-02) active:bg-(--base-03) focus-visible:outline-2 focus-visible:outline-(--accent) p-1.5 rounded-md transition-colors"
                            title="Cancel download"
                            aria-label={`Cancel download of ${e.filename}`}
                        >
                            <CircleStop size={16} />
                        </button>
                    )}
                    {revealable && (
                        <button
                            type="button"
                            onClick={reveal}
                            className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) hover:bg-(--base-02) active:bg-(--base-03) focus-visible:outline-2 focus-visible:outline-(--accent) p-1.5 rounded-md transition-colors"
                            title="Show in folder"
                            aria-label={`Show ${e.filename} in folder`}
                        >
                            <FolderOpen size={16} />
                        </button>
                    )}
                    {!running && (
                        <button
                            type="button"
                            onClick={() => downloadStore.remove(e.id)}
                            className="text-(--base-06) hover:text-(--base-09) active:text-(--base-07) hover:bg-(--base-02) active:bg-(--base-03) focus-visible:outline-2 focus-visible:outline-(--accent) p-1.5 rounded-md transition-colors"
                            title="Dismiss"
                            aria-label={`Dismiss ${e.filename}`}
                        >
                            <X size={16} />
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
                <span
                    className={`truncate text-right ${e.state === 'failed' ? 'text-(--error-light)' : e.note ? 'text-(--warning-light)' : ''}`}
                    title={e.error ?? e.note}
                >
                    {running
                        ? e.cancelling
                            ? 'Cancelling'
                            : e.note
                                ? e.note
                                : speed > 0
                                    ? `${formatBytes(speed)}/s${eta !== null ? ` · ${formatEta(eta)}` : ''}`
                                    : e.reported ? 'Waiting for data' : 'Starting'
                        : e.state === 'done'
                            ? 'Completed'
                            : e.state === 'cancelled'
                                ? 'Cancelled'
                                : `Failed: ${e.error}`}
                </span>
            </div>
        </li>
    );
}
