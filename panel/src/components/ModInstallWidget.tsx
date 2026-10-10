"use client";

import { useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { Package, X, CircleCheck, CircleAlert, Trash2, Loader2, Send } from 'lucide-react';
import { useModInstalls } from '@/lib/modInstallManager';
import { isPending, jobLabel, type InstallJob } from '@/lib/modInstallJobs';

// Navbar entry for mod installs, next to the upload widget and built the same
// way. Hidden while there is nothing to show.
export default function ModInstallWidget() {
    const mgr = useModInstalls();
    const [open, setOpen] = useState(false);
    const wrapRef = useRef<HTMLDivElement>(null);

    useEffect(() => {
        if (!open) return;
        const onClick = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
        };
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
        document.addEventListener('click', onClick);
        document.addEventListener('keydown', onKey);
        return () => {
            document.removeEventListener('click', onClick);
            document.removeEventListener('keydown', onKey);
        };
    }, [open]);

    if (mgr.jobs.length === 0) return null;

    const active = mgr.jobs.filter(isPending).length;
    const failed = mgr.jobs.some(j => j.state === 'failed' || j.state === 'unknown');
    const label = active > 0
        ? `${active} mod install${active !== 1 ? 's' : ''} running`
        : 'Mod installs finished';
    const tone = active > 0
        ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border)'
        : failed
            ? 'bg-(--warning-ghost) text-(--warning-light) border-(--warning)/30'
            : 'bg-(--success)/10 text-(--success-light) border-(--success)/20';

    return (
        <div ref={wrapRef} className="relative mr-2">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                title={label}
                aria-label={label}
                aria-expanded={open}
                className={`relative flex items-center justify-center w-9 h-9 rounded-md transition-colors border hover:brightness-110 focus-visible:outline-2 focus-visible:outline-(--accent) ${
                    open ? 'bg-(--base-03) border-(--base-04) text-(--base-09)' : tone
                }`}
            >
                {active > 0 ? <Loader2 size={16} className="animate-spin motion-reduce:animate-none" /> : <Package size={16} />}
                {active > 0 && (
                    <span className="absolute -top-1 -right-1 min-w-4 h-4 px-1 rounded-full bg-(--accent-light) text-(--base-00) text-[10px] font-mono font-bold flex items-center justify-center leading-none">
                        {active}
                    </span>
                )}
            </button>

            {open && (
                <div className="dropdown-menu right-0 mt-3 w-96 animate-fade-in origin-top-right" role="region" aria-label="Mod installs">
                    <div className="flex items-center justify-between px-4 py-2 border-b border-(--base-03)">
                        <div className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                            Mod installs ({mgr.jobs.length})
                        </div>
                        <div className="flex items-center gap-1">
                            {mgr.jobs.some(j => !isPending(j)) && (
                                <button
                                    type="button"
                                    onClick={mgr.clearFinished}
                                    className="text-(--base-06) hover:text-(--base-09) p-1 rounded"
                                    title="Clear finished entries"
                                    aria-label="Clear finished entries"
                                >
                                    <Trash2 size={11} />
                                </button>
                            )}
                            <button
                                type="button"
                                onClick={() => setOpen(false)}
                                className="text-(--base-06) hover:text-(--base-09) p-1 rounded"
                                aria-label="Close"
                            >
                                <X size={12} />
                            </button>
                        </div>
                    </div>
                    <ul className="max-h-[60vh] overflow-y-auto" aria-live="polite">
                        {[...mgr.jobs].reverse().map(j => (
                            <InstallRow key={j.id} job={j} onDismiss={() => mgr.dismiss(j.id)} />
                        ))}
                    </ul>
                </div>
            )}
        </div>
    );
}

function InstallRow({ job, onDismiss }: { job: InstallJob; onDismiss: () => void }) {
    const pending = isPending(job);
    const tone = job.state === 'installed'
        ? 'text-(--success-light)'
        : job.state === 'failed' || job.state === 'unknown'
            ? 'text-(--warning-light)'
            : 'text-(--base-06)';
    return (
        <li className="px-3 py-2.5 border-b border-(--base-03) last:border-b-0">
            <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                    <div className="text-sm text-(--base-09) truncate" title={job.title}>{job.title}</div>
                    <Link
                        href={`/servers/${job.serverId}/content`}
                        className="text-[11px] text-(--base-06) hover:text-(--base-09) truncate block"
                    >
                        {job.serverName}
                    </Link>
                </div>
                <div className="shrink-0 flex items-center gap-1">
                    {job.state === 'installed' && <CircleCheck size={14} className="text-(--success-light)" />}
                    {(job.state === 'failed' || job.state === 'unknown') && <CircleAlert size={14} className="text-(--warning-light)" />}
                    {job.state === 'sent' && <Send size={13} className="text-(--base-06)" />}
                    {!pending && (
                        <button
                            type="button"
                            onClick={onDismiss}
                            className="text-(--base-06) hover:text-(--base-09) p-1 rounded"
                            title="Dismiss"
                            aria-label={`Dismiss ${job.title}`}
                        >
                            <X size={12} />
                        </button>
                    )}
                </div>
            </div>
            <InstallBar job={job} />
            <div className={`mt-1 text-[11px] ${tone}`}>
                {jobLabel(job)}
                {job.state === 'sent' && ' - this node does not report back; check the content list in a moment.'}
                {job.state === 'installed' && ' - restart the server to load it.'}
                {job.message && (job.state === 'failed' || job.state === 'unknown') && <> - {job.message}</>}
            </div>
        </li>
    );
}

// The node reports no byte counts, so a pending install is an indeterminate bar
// rather than a percentage the panel would have to invent.
export function InstallBar({ job }: { job: InstallJob }) {
    const pending = isPending(job);
    const color = job.state === 'installed'
        ? 'bg-(--success-light)'
        : job.state === 'failed' || job.state === 'unknown'
            ? 'bg-(--warning-light)'
            : 'bg-(--accent-light)';
    return (
        <div
            className="mt-1.5 h-1 rounded-full bg-(--base-03) overflow-hidden"
            role="progressbar"
            aria-label={jobLabel(job)}
            aria-busy={pending}
        >
            <div
                className={`h-full rounded-full ${color} ${pending ? 'animate-pulse motion-reduce:animate-none' : ''}`}
                style={{ width: job.state === 'sending' ? '15%' : pending ? '60%' : '100%', transition: 'width 300ms ease-out' }}
            />
        </div>
    );
}
