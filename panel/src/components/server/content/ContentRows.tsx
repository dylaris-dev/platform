"use client";

import type { MouseEvent, KeyboardEvent } from 'react';
import { Package, Download, RefreshCw, Trash2, AlertTriangle, MoreHorizontal, RotateCcw, Power, PowerOff, Lock, LockOpen, ExternalLink } from 'lucide-react';
import type { ModrinthSearchHit, InstalledMod, ModHistoryEntry } from '@/lib/api/modrinth';
import { serverBucket, serverBucketLabel, serverBucketTitle, type ModSides } from '@/lib/modSide';
import type { ModStatus } from '@/lib/modVersionCompare';
import { isOnServer } from '@/lib/installedState';
import { isPending, jobLabel, type InstallJob } from '@/lib/modInstallJobs';
import { InstallBar } from '@/components/ModInstallWidget';
import { ModrinthProjectActions } from '@/components/mods/ModrinthLibrary';
import ProjectIcon from '@/components/modpacks/ProjectIcon';
import Menu, { type MenuEntry } from '@/components/ui/Menu';
import { fmtDateTime } from '@/lib/time';

// 'checking' is a UI-only state (the matching-version fetch for an installed
// row is still in flight) layered on top of the pure ModStatus.
export type RowStatus = ModStatus | 'checking';

type Tone = 'success' | 'warning' | 'error' | 'accent';

export function Tag({ tone, title, children }: { tone?: Tone; title?: string; children: React.ReactNode }) {
    return <span className="srv-content-tag" data-tone={tone} title={title}>{children}</span>;
}

// The server-side bucket tag ("Server only", ...); none where Modrinth does
// not say enough. Client only is a warning: it only appears on Installed.
export function BucketTag({ sides }: { sides?: ModSides | null }) {
    const b = sides ? serverBucket(sides) : null;
    if (!sides || !b) return null;
    const warning = b === 'client_only';
    return (
        <Tag tone={warning ? 'warning' : undefined} title={serverBucketTitle(sides) ?? undefined}>
            {warning && <AlertTriangle size={11} aria-hidden="true" />}
            {serverBucketLabel(b)}
        </Tag>
    );
}

export function BrowseRow({
    hit, status, sides, updateChannel, installed, busy, job, selected, fitsVersion,
    onOpen, onInstall, onRemove, onUpdate,
}: {
    hit: ModrinthSearchHit;
    status: RowStatus;
    sides: ModSides | null;
    updateChannel?: string;
    installed: boolean;
    busy: boolean;
    job?: InstallJob;
    selected: boolean;
    /** The server's Minecraft version when this project lists it; shown as the compatibility badge. */
    fitsVersion?: string;
    onOpen: () => void;
    onInstall: () => void;
    onRemove: () => void;
    onUpdate: () => void;
}) {
    // The row opens the detail drawer on click but hosts real buttons, so it
    // is a div with button semantics and the actions stop propagation.
    const handleKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
        if (e.target !== e.currentTarget) return;
        if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            onOpen();
        }
    };
    // The install job outlives the click: busy covers resolving the version,
    // the job covers Core and the node until they answer.
    const jobBusy = !!job && isPending(job);
    const actionBusy = busy || jobBusy;
    const stopAnd = (fn: () => void) => (e: MouseEvent) => {
        e.stopPropagation();
        fn();
    };
    const spinner = <RefreshCw size={14} className="animate-spin motion-reduce:animate-none" />;

    return (
        <div
            role="button"
            tabIndex={0}
            onClick={onOpen}
            onKeyDown={handleKeyDown}
            data-clickable="true"
            data-selected={selected}
            aria-label={`Open ${hit.title}`}
            className="srv-content-row"
        >
            {hit.icon_url ? (
                // eslint-disable-next-line @next/next/no-img-element
                <img src={hit.icon_url} alt="" className="srv-content-icon" />
            ) : (
                <div className="srv-content-icon"><Package size={18} /></div>
            )}

            <div className="min-w-0 flex-1">
                <div className="flex min-w-0 items-baseline gap-2">
                    <span className="truncate text-sm font-medium text-(--base-09)">{hit.title}</span>
                    <span className="hidden shrink-0 text-xs text-(--base-06) sm:inline">by {hit.author}</span>
                </div>
                <p className="mt-0.5 line-clamp-1 text-[13px] text-(--base-07)">{hit.description}</p>
                <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1">
                    <span className="srv-num inline-flex items-center gap-1 text-xs text-(--base-06)">
                        <Download size={12} aria-hidden="true" />
                        {hit.downloads.toLocaleString()}
                    </span>
                    {fitsVersion && <Tag tone="success" title={`Lists a build for Minecraft ${fitsVersion}`}>Fits {fitsVersion}</Tag>}
                    <BucketTag sides={sides} />
                </div>
                {job && jobBusy && <InstallBar job={job} />}
                {job && (job.state === 'failed' || job.state === 'unknown') && job.message && (
                    <p className="mt-1.5 flex items-start gap-1 text-xs text-(--warning-light)">
                        <AlertTriangle size={12} className="mt-0.5 shrink-0" aria-hidden="true" />
                        <span>{job.message}</span>
                    </p>
                )}
            </div>

            <div className="flex shrink-0 items-center gap-2" onClick={e => e.stopPropagation()}>
                <ModrinthProjectActions projectId={hit.project_id} title={hit.title} />
                {status === 'up-to-date' && <Tag tone="success">Installed</Tag>}
                {status === 'checking' && <Tag>Checking…</Tag>}

                {status === 'not-installed' && (
                    <button onClick={stopAnd(onInstall)} disabled={actionBusy} aria-busy={actionBusy} className="btn btn-primary btn-sm">
                        {actionBusy ? spinner : <Download size={14} />}
                        {job && jobBusy ? jobLabel(job) : job?.state === 'failed' || job?.state === 'unknown' ? 'Try again' : 'Install'}
                    </button>
                )}
                {status === 'update-available' && (
                    <button onClick={stopAnd(onUpdate)} disabled={actionBusy} aria-busy={actionBusy} className="btn btn-primary btn-sm">
                        {actionBusy ? spinner : <Download size={14} />}
                        {job && jobBusy ? jobLabel(job) : updateChannel && updateChannel !== 'release' ? `Update (${updateChannel})` : 'Update'}
                    </button>
                )}
                {installed && (
                    <Menu
                        trigger={
                            <button type="button" aria-label={`More actions for ${hit.title}`} disabled={busy} className="btn btn-secondary btn-icon btn-sm">
                                {busy ? spinner : <MoreHorizontal size={14} />}
                            </button>
                        }
                        items={[{ label: 'Remove', icon: <Trash2 size={14} />, danger: true, onSelect: onRemove }]}
                    />
                )}
            </div>
        </div>
    );
}

export function InstalledRow({
    m, iconUrl, sides, showSides, mismatch, busy, installJob, downloading, history,
    onSwap, onRollback, onToggleEnabled, onTogglePaused, onUninstall,
}: {
    m: InstalledMod;
    iconUrl?: string;
    sides?: ModSides | null;
    showSides: boolean;
    mismatch: string | null;
    busy: boolean;
    /** The job this session follows for the row, shown as a progress bar. */
    installJob?: InstallJob | null;
    /** Core refuses a Remove while the download may still run. */
    downloading: boolean;
    history: ModHistoryEntry[];
    onSwap: () => void;
    onRollback: (entry: ModHistoryEntry) => void;
    onToggleEnabled: () => void;
    onTogglePaused: () => void;
    onUninstall: () => void;
}) {
    const name = m.title || m.fileName;
    const items: MenuEntry[] = [];
    // Only the builds this server actually ran. Modrinth's own version list
    // offers everything; what it cannot tell you is which build you had before
    // the update that broke something.
    if (!m.disabled) {
        for (const h of history) {
            items.push({
                id: `rb-${h.id}`,
                label: `Roll back to ${h.fileName} (replaced ${fmtDateTime(h.replacedAt)})`,
                icon: <RotateCcw size={14} />,
                disabled: busy,
                onSelect: () => onRollback(h),
            });
        }
        if (history.length) items.push({ type: 'separator', id: 'sep-history' });
    }
    if (isOnServer(m)) {
        items.push({
            label: m.disabled ? 'Switch on (next restart)' : 'Switch off (next restart)',
            icon: m.disabled ? <PowerOff size={14} /> : <Power size={14} />,
            disabled: busy,
            onSelect: onToggleEnabled,
        });
    }
    if (!m.disabled) {
        items.push({
            label: m.updatesPaused ? 'Include in Update all again' : 'Pause updates',
            icon: m.updatesPaused ? <Lock size={14} /> : <LockOpen size={14} />,
            disabled: busy,
            onSelect: onTogglePaused,
        });
    }
    items.push({
        label: 'View on Modrinth',
        icon: <ExternalLink size={14} />,
        href: `https://modrinth.com/project/${m.modrinthProjectSlug || m.modrinthProjectId}`,
        external: true,
    });
    items.push({ type: 'separator', id: 'sep-remove' });
    items.push({
        label: downloading ? 'Remove (still installing)' : 'Remove',
        icon: <Trash2 size={14} />,
        danger: true,
        disabled: downloading,
        onSelect: onUninstall,
    });

    return (
        <article className="srv-content-row" data-dim={!!m.disabled}>
            <ProjectIcon url={iconUrl} name={name} className="size-11" />
            <div className="min-w-0 flex-1">
                <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="truncate text-sm font-medium text-(--base-09)">{name}</span>
                    {m.disabled && <Tag title="The jar is in .disabled/ and is not loaded">Off</Tag>}
                    {m.updatesPaused && !m.disabled && <Tag title="Update all skips this mod">Updates paused</Tag>}
                    {showSides && <BucketTag sides={sides} />}
                    {mismatch && (
                        <Tag tone="warning">
                            <AlertTriangle size={11} aria-hidden="true" />
                            {mismatch}
                        </Tag>
                    )}
                </div>
                <div className="srv-num mt-1 truncate text-xs text-(--base-06)">
                    {m.fileName}
                    {m.status === 'installing'
                        ? ' · installing, the node is downloading it'
                        : m.status === 'failed'
                            ? ''
                            : ` · installed ${fmtDateTime(m.installedAt)}`}
                </div>
                {installJob && <InstallBar job={installJob} />}
                {m.status === 'failed' && (
                    <div className="mt-1.5 flex items-start gap-1.5 text-xs text-(--warning-light)">
                        <AlertTriangle size={12} className="mt-0.5 shrink-0" aria-hidden="true" />
                        <span>
                            This did not install. The server is still running whatever it had
                            before.{m.statusMessage ? ` ${m.statusMessage}` : ''}
                        </span>
                    </div>
                )}
            </div>
            <div className="flex shrink-0 items-center gap-2">
                {/* A switched-off mod offers only switching it back on and
                    removing it: Core refuses to install over its jar. */}
                {!m.disabled && mismatch && (
                    <button onClick={onSwap} disabled={busy} aria-busy={busy} className="btn btn-primary btn-sm">
                        <RefreshCw size={14} className={busy ? 'animate-spin motion-reduce:animate-none' : ''} />
                        <span className="hidden sm:inline">Swap to the matching build</span>
                        <span className="sm:hidden">Swap</span>
                    </button>
                )}
                <ModrinthProjectActions projectId={m.modrinthProjectId} title={name} />
                <Menu
                    trigger={
                        <button type="button" aria-label={`Actions for ${name}`} className="btn btn-secondary btn-icon btn-sm">
                            {busy ? <RefreshCw size={14} className="animate-spin motion-reduce:animate-none" /> : <MoreHorizontal size={14} />}
                        </button>
                    }
                    className="w-80"
                    items={items}
                />
            </div>
        </article>
    );
}
