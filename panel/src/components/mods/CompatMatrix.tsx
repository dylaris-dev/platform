'use client';

import { useState } from 'react';
import { ChevronRight, ExternalLink, CircleAlert, Loader2, RefreshCw } from 'lucide-react';
import '@/styles/server-content.css';
import Segmented from '@/components/ui/Segmented';
import Select from '@/components/ui/Select';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import type {
    CompatMatrix as CompatMatrixData,
    CompatMode,
    CompatSide,
    CompatStatus,
    CompatVersion,
} from '@/lib/api/modcompat';

// The cross-Minecraft-version availability view. Rendered by both the modpack
// builder and a modded server: the data shape is identical, only the action on
// a row differs, which is what renderAction is for.
//
// It reports AVAILABILITY, not compatibility. "Green" means every mod has a
// version published for that Minecraft version and loader. It cannot mean the
// pack still works: a mod can be present and still be broken by a dependency it
// no longer matches. The copy says availability everywhere for that reason.

const SIDE_LABELS: Record<CompatSide, string> = {
    client: 'Client only',
    server: 'Server only',
    both: 'Client + server',
};

const STATUS_DOT: Record<CompatStatus, string> = {
    green: 'bg-(--success-light)',
    orange: 'bg-(--warning-light)',
    red: 'bg-(--error-light)',
    empty: 'bg-(--base-05)',
};

const STATUS_TEXT: Record<CompatStatus, string> = {
    green: 'text-(--success-light)',
    orange: 'text-(--warning-light)',
    red: 'text-(--error-light)',
    empty: 'text-(--base-06)',
};

const MODES: { value: CompatMode; label: string; hint: string }[] = [
    {
        value: 'all-newer',
        label: 'Everything newer',
        hint: 'Every release after the current one, including the rest of its own line',
    },
    {
        value: 'newer-lines',
        label: 'Newer lines only',
        hint: 'Skips the rest of the current line and starts at the next one',
    },
    { value: 'specific', label: 'One version', hint: 'Check a single Minecraft version' },
];

export interface CompatMatrixProps {
    data: CompatMatrixData | null;
    loading: boolean;
    error?: string;
    mode: CompatMode;
    onModeChange: (mode: CompatMode) => void;
    /** Only used in 'specific' mode. */
    specific: string;
    onSpecificChange: (mc: string) => void;
    /** Candidate versions for the 'specific' picker. */
    versionOptions: string[];
    onRefresh: () => void;
    /** What to offer on a version row. The caller decides: create a build, or move the server. */
    renderAction?: (version: CompatVersion) => React.ReactNode;
}

export default function CompatMatrix({
    data,
    loading,
    error,
    mode,
    onModeChange,
    specific,
    onSpecificChange,
    versionOptions,
    onRefresh,
    renderAction,
}: CompatMatrixProps) {
    return (
        <div className="flex flex-col gap-4">
            <div className="flex flex-wrap items-end justify-between gap-3">
                <div className="flex flex-col gap-[5px]">
                    <span className="input-label">What to check</span>
                    <Segmented
                        ariaLabel="Version range"
                        value={mode}
                        onChange={onModeChange}
                        options={MODES.map(m => ({ id: m.value, label: m.label, hint: m.hint }))}
                    />
                </div>

                {mode === 'specific' && (
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="compat-specific">Minecraft version</label>
                        <Select
                            id="compat-specific"
                            value={specific}
                            onChange={onSpecificChange}
                            placeholder="Pick a version"
                            options={versionOptions.map(v => ({ value: v, label: v }))}
                        />
                    </div>
                )}

                <button
                    type="button"
                    onClick={onRefresh}
                    disabled={loading}
                    className="btn btn-secondary inline-flex items-center gap-2"
                >
                    {loading ? <Loader2 size={14} className="animate-spin" /> : <RefreshCw size={14} />}
                    {loading ? 'Checking' : 'Check availability'}
                </button>
            </div>

            {error && (
                <p className="text-sm text-(--error-light) flex items-start gap-2">
                    <CircleAlert size={14} className="mt-0.5 shrink-0" /> {error}
                </p>
            )}

            {loading && !data && <MatrixSkeleton />}

            {data && <MatrixBody data={data} renderAction={renderAction} />}
        </div>
    );
}

function MatrixSkeleton() {
    return (
        <div className="flex flex-col gap-2" aria-hidden>
            {[0, 1, 2].map(i => (
                <div key={i} className="srv-surface p-5 flex flex-col gap-3">
                    <Skeleton className="h-4 w-24" />
                    <SkeletonText width="w-full" />
                    <SkeletonText width="w-2/3" />
                </div>
            ))}
        </div>
    );
}

function MatrixBody({
    data,
    renderAction,
}: {
    data: CompatMatrixData;
    renderAction?: (version: CompatVersion) => React.ReactNode;
}) {
    // The newest line is the one an operator is most likely heading for, so it
    // starts open and the older ones stay collapsed.
    const [openLines, setOpenLines] = useState<Record<string, boolean>>(() =>
        data.lines.length > 0 ? { [data.lines[data.lines.length - 1].line]: true } : {},
    );

    if (data.lines.length === 0) {
        return (
            <p className="text-sm text-(--base-06)">
                {data.checked === 0
                    ? 'Nothing here carries a Modrinth identity, so there is nothing to check.'
                    : `Nothing newer than Minecraft ${data.current} to check.`}
            </p>
        );
    }

    return (
        <div className="flex flex-col gap-3">
            <p className="text-[13px] text-(--base-06)">
                Checked <span className="srv-num text-(--base-08)">{data.checked}</span> items against{' '}
                <span className="srv-num text-(--base-08)">{data.loader}</span>, currently on{' '}
                <span className="srv-num text-(--base-08)">{data.current}</span>. Availability only: a mod that
                exists for a version can still be broken by a dependency.
            </p>

            {data.lines.map(line => {
                const open = !!openLines[line.line];
                return (
                    <div key={line.line} className="srv-content-matrix-line">
                        <button
                            type="button"
                            onClick={() => setOpenLines(s => ({ ...s, [line.line]: !s[line.line] }))}
                            aria-expanded={open}
                            className="srv-content-matrix-head"
                        >
                            <ChevronRight
                                size={14}
                                className={`text-(--base-06) transition-transform ${open ? 'rotate-90' : ''}`}
                            />
                            <span className="srv-num text-sm font-medium text-(--base-09)">{line.line}</span>
                            <span className="srv-num ml-auto flex items-center gap-3 text-xs">
                                {line.green > 0 && <LineCount status="green" n={line.green} />}
                                {line.orange > 0 && <LineCount status="orange" n={line.orange} />}
                                {line.red > 0 && <LineCount status="red" n={line.red} />}
                            </span>
                        </button>

                        {open && (
                            <div>
                                {line.versions.map(v => (
                                    <VersionRow key={v.minecraft} version={v} renderAction={renderAction} />
                                ))}
                            </div>
                        )}
                    </div>
                );
            })}
        </div>
    );
}

function LineCount({ status, n }: { status: CompatStatus; n: number }) {
    return (
        <span className={`inline-flex items-center gap-1.5 ${STATUS_TEXT[status]}`}>
            <span className={`w-1.5 h-1.5 rounded-full ${STATUS_DOT[status]}`} />
            {n}
        </span>
    );
}

function VersionRow({
    version,
    renderAction,
}: {
    version: CompatVersion;
    renderAction?: (version: CompatVersion) => React.ReactNode;
}) {
    const [open, setOpen] = useState(false);
    const expandable = version.missing.length > 0;

    return (
        <div className="srv-content-matrix-row">
            <div className="flex flex-wrap items-center gap-3 px-5 py-2.5">
                <button
                    type="button"
                    disabled={!expandable}
                    onClick={() => setOpen(o => !o)}
                    aria-expanded={expandable ? open : undefined}
                    title={expandable ? 'Show what would be lost' : 'Everything is available here'}
                    className="inline-flex items-center gap-2 rounded-sm px-1 -mx-1 disabled:cursor-default hover:enabled:text-(--base-09) focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] transition-colors"
                >
                    <ChevronRight
                        size={12}
                        className={`transition-transform ${open ? 'rotate-90' : ''} ${expandable ? 'text-(--base-06)' : 'opacity-0'}`}
                    />
                    <span className={`w-2 h-2 rounded-full shrink-0 ${STATUS_DOT[version.status]}`} />
                    <span className="srv-num text-sm text-(--base-08)">{version.minecraft}</span>
                </button>

                <div className="flex flex-wrap items-center gap-1.5">
                    {(['both', 'server', 'client'] as CompatSide[]).map(side => (
                        <SideChip key={side} side={side} bucket={version.buckets[side]} />
                    ))}
                </div>

                <div className="ml-auto">{renderAction?.(version)}</div>
            </div>

            {open && expandable && (
                <ul className="px-5 pb-3 pt-1 flex flex-col gap-1.5">
                    {version.missing.map(m => (
                        <li key={m.key} className="flex flex-wrap items-center gap-2 text-xs">
                            <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${m.side === 'both' ? 'bg-(--error-light)' : 'bg-(--warning-light)'}`} />
                            <a
                                href={`https://modrinth.com/mod/${encodeURIComponent(m.slug || m.projectId)}`}
                                target="_blank"
                                rel="noopener noreferrer"
                                className="inline-flex items-center gap-1 text-(--base-08) hover:text-(--accent-light) underline decoration-dotted underline-offset-2 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] rounded-sm transition-colors"
                            >
                                {m.title || m.projectId}
                                <ExternalLink size={10} />
                            </a>
                            {m.currentVersion && (
                                <span className="srv-num text-(--base-06)">on {m.currentVersion}</span>
                            )}
                            <span className="text-(--base-06)">{SIDE_LABELS[m.side]}</span>
                            <span className="text-(--base-06)">
                                {m.reason === 'unresolvable'
                                    ? 'not listed on Modrinth any more, so this could not be checked'
                                    : 'no version published here'}
                            </span>
                        </li>
                    ))}
                </ul>
            )}
        </div>
    );
}

function SideChip({ side, bucket }: { side: CompatSide; bucket?: { total: number; available: number; status: CompatStatus } }) {
    // An empty bucket is not rendered at all: a pack with no client-only mods
    // should not show a grey "Client only 0/0" that reads like a warning.
    if (!bucket || bucket.total === 0) return null;
    const tone = bucket.status === 'green' ? 'success' : bucket.status === 'red' ? 'error' : 'warning';
    return (
        <span
            className="srv-content-tag srv-num"
            data-tone={tone}
            title={`${bucket.available} of ${bucket.total} ${SIDE_LABELS[side].toLowerCase()} items are available`}
        >
            {SIDE_LABELS[side]} {bucket.available}/{bucket.total}
        </span>
    );
}
