"use client";

import React, { useEffect, useRef, useState } from 'react';
import { Upload, X, CircleCheck, CircleAlert, Trash2, Loader2 } from 'lucide-react';
import {
    useUploadManager,
    jobSpeedBps,
    jobEtaSeconds,
    UploadJob,
} from '@/lib/uploadManager';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function formatBytes(b: number): string {
    if (b < 1024) return `${b} B`;
    const units = ['KB', 'MB', 'GB', 'TB'];
    let v = b / 1024;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i++;
    }
    return `${v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)} ${units[i]}`;
}

function formatBytesPerSec(bps: number): string {
    if (bps <= 0) return '0 B/s';
    return `${formatBytes(bps)}/s`;
}

function formatEta(seconds: number | null): string {
    if (seconds === null) return '…';
    if (seconds < 1) return '<1s';
    if (seconds < 60) return `${seconds}s`;
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    return `${h}h ${m}m`;
}

function progress(job: UploadJob): number {
    if (job.status === 'done') return 1;
    if (job.size <= 0) return 0;
    return Math.max(0, Math.min(1, job.sentBytes / job.size));
}

// ---------------------------------------------------------------------------
// Circular progress ring used inside the navbar button
// ---------------------------------------------------------------------------

interface RingProps {
    progress: number;   // 0..1
    size?: number;
    stroke?: number;
    children?: React.ReactNode;
}

export function ProgressRing({ progress, size = 24, stroke = 2.5, children }: RingProps) {
    const r = (size - stroke) / 2;
    const circumference = 2 * Math.PI * r;
    const offset = circumference * (1 - Math.max(0, Math.min(1, progress)));
    const cx = size / 2;
    return (
        <span className="relative inline-flex items-center justify-center" style={{ width: size, height: size }}>
            <svg width={size} height={size} className="absolute inset-0">
                <circle
                    cx={cx}
                    cy={cx}
                    r={r}
                    fill="none"
                    stroke="rgba(255,255,255,0.12)"
                    strokeWidth={stroke}
                />
                <circle
                    cx={cx}
                    cy={cx}
                    r={r}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth={stroke}
                    strokeLinecap="round"
                    strokeDasharray={circumference}
                    strokeDashoffset={offset}
                    transform={`rotate(-90 ${cx} ${cx})`}
                    style={{ transition: 'stroke-dashoffset 250ms ease-out' }}
                />
            </svg>
            {children && <span className="relative z-10 leading-none">{children}</span>}
        </span>
    );
}

// ---------------------------------------------------------------------------
// Widget — sits in the navbar between Notifications and Settings
// ---------------------------------------------------------------------------

export default function UploadManagerWidget() {
    const mgr = useUploadManager();
    const [open, setOpen] = useState(false);
    const wrapRef = useRef<HTMLDivElement>(null);

    // Auto-open the panel briefly when the first upload starts so the
    // user can confirm it's tracked, then let them collapse it.
    const prevActiveRef = useRef(0);
    useEffect(() => {
        if (prevActiveRef.current === 0 && mgr.activeCount > 0) {
            setOpen(true);
        }
        prevActiveRef.current = mgr.activeCount;
    }, [mgr.activeCount]);

    useEffect(() => {
        const handler = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
        };
        document.addEventListener('click', handler);
        return () => document.removeEventListener('click', handler);
    }, []);

    if (mgr.jobs.length === 0) return null; // hidden when nothing to show

    const hasActive = mgr.hasActive;
    const tone = hasActive
        ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border) hover:bg-(--accent-ghost)/80'
        : 'bg-(--success)/10 text-(--success-light) border-(--success)/20 hover:bg-(--success)/15';

    const label = hasActive
        ? `${mgr.activeCount} upload${mgr.activeCount !== 1 ? 's' : ''} active`
        : 'Uploads finished';

    return (
        <div ref={wrapRef} className="relative mr-2">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                title={label}
                className={`relative flex items-center justify-center w-9 h-9 rounded-md transition-colors border ${
                    open
                        ? 'bg-(--base-03) border-(--base-04) text-(--base-09)'
                        : tone
                }`}
            >
                {hasActive ? (
                    <ProgressRing progress={mgr.aggregateProgress}>
                        <Upload size={12} />
                    </ProgressRing>
                ) : (
                    <Upload size={16} />
                )}
                {mgr.activeCount > 0 && (
                    <span className="absolute -top-1 -right-1 min-w-4 h-4 px-1 rounded-full bg-(--accent-light) text-(--base-00) text-[10px] font-mono font-bold flex items-center justify-center leading-none">
                        {mgr.activeCount}
                    </span>
                )}
            </button>

            {open && <UploadPanel onClose={() => setOpen(false)} />}
        </div>
    );
}

// ---------------------------------------------------------------------------
// Expanded panel — list of jobs with live speed/ETA + cancel
// ---------------------------------------------------------------------------

function UploadPanel({ onClose }: { onClose: () => void }) {
    const mgr = useUploadManager();
    // Periodic re-render so speed/ETA tick smoothly even when no new chunk
    // landed in the last second (e.g. for the last chunk of a file).
    const [, force] = useState(0);
    useEffect(() => {
        const t = setInterval(() => force(n => n + 1), 1000);
        return () => clearInterval(t);
    }, []);

    return (
        <div className="dropdown-menu right-0 mt-3 w-96 animate-fade-in origin-top-right">
            <div className="flex items-center justify-between px-4 py-2 border-b border-(--base-03)">
                <div className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                    Uploads ({mgr.jobs.length})
                </div>
                <div className="flex items-center gap-1">
                    {mgr.jobs.some(j => j.status !== 'running' && j.status !== 'cancelling') && (
                        <button
                            type="button"
                            onClick={mgr.clearFinished}
                            className="text-(--base-06) hover:text-(--base-09) p-1 rounded"
                            title="Clear finished entries"
                        >
                            <Trash2 size={11} />
                        </button>
                    )}
                    <button
                        type="button"
                        onClick={onClose}
                        className="text-(--base-06) hover:text-(--base-09) p-1 rounded"
                    >
                        <X size={12} />
                    </button>
                </div>
            </div>
            <div className="max-h-[60vh] overflow-y-auto">
                {mgr.jobs.length === 0 ? (
                    <div className="px-4 py-6 text-sm text-(--base-06) text-center">
                        No uploads in flight.
                    </div>
                ) : (
                    mgr.jobs.map(j => <UploadRow key={j.id} job={j} />)
                )}
            </div>
        </div>
    );
}

function UploadRow({ job }: { job: UploadJob }) {
    const mgr = useUploadManager();
    const p = progress(job);
    const bps = jobSpeedBps(job);
    const eta = jobEtaSeconds(job);

    const isLive =
        job.status === 'running' ||
        job.status === 'retrying' ||
        job.status === 'cancelling';
    const isRetrying = job.status === 'retrying';

    const barColor =
        job.status === 'error'
            ? 'bg-(--error-light)'
            : job.status === 'cancelled' || job.status === 'cancelling'
                ? 'bg-(--warning-light)'
                : job.status === 'done'
                    ? 'bg-(--success-light)'
                    : isRetrying
                        ? 'bg-(--warning-light)'
                        : 'bg-(--accent-light)';

    return (
        <div className="px-3 py-2.5 border-b border-(--base-03) last:border-b-0">
            <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                    <div className="text-sm text-(--base-09) truncate" title={job.filename}>
                        {job.filename}
                    </div>
                    <div className="text-[11px] text-(--base-06) font-mono truncate">
                        {job.serverName ?? job.serverUuid} / {job.path || '.'}
                    </div>
                </div>
                <div className="shrink-0 flex items-center gap-1">
                    {isRetrying && <Loader2 size={13} className="animate-spin text-(--warning-light)" />}
                    {job.status === 'done' && <CircleCheck size={14} className="text-(--success-light)" />}
                    {job.status === 'error' && <CircleAlert size={14} className="text-(--error-light)" />}
                    {isLive && (
                        <button
                            type="button"
                            onClick={() => mgr.requestCancel(job.id)}
                            disabled={job.status === 'cancelling'}
                            className="text-(--base-06) hover:text-(--error-light) p-1 rounded disabled:opacity-40"
                            title={job.status === 'cancelling' ? 'Cancelling…' : 'Cancel upload'}
                        >
                            <X size={12} />
                        </button>
                    )}
                </div>
            </div>

            {isRetrying && (
                // Pulled-out warning row so the retry state is visible
                // without competing with the speed/ETA line.
                <div className="mt-1.5 flex items-center gap-1.5 text-[11px] text-(--warning-light) font-medium">
                    <Loader2 size={11} className="animate-spin" />
                    <span>Reconnecting… retrying upload</span>
                </div>
            )}

            <div className="mt-1.5 h-1 rounded-full bg-(--base-03) overflow-hidden">
                <div
                    className={`h-full rounded-full ${barColor}`}
                    style={{ width: `${Math.round(p * 100)}%`, transition: 'width 200ms ease-out' }}
                />
            </div>

            <div className="mt-1 flex items-center justify-between gap-2 font-mono text-[10px] text-(--base-06)">
                <span className="tabular-nums">
                    {Math.round(p * 100)}% — {formatBytes(job.sentBytes)} / {formatBytes(job.size)}
                </span>
                <span className="tabular-nums">
                    {isRetrying ? (
                        'retrying…'
                    ) : isLive ? (
                        <>
                            {formatBytesPerSec(bps)} · ETA {formatEta(eta)}
                        </>
                    ) : job.status === 'done' ? (
                        'completed'
                    ) : job.status === 'cancelled' || job.status === 'cancelling' ? (
                        'cancelled'
                    ) : (
                        job.errorMessage ?? 'failed'
                    )}
                </span>
            </div>
        </div>
    );
}
