"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';

import { FileText, Filter, Loader2, ShieldCheck, ShieldOff, AlertTriangle, ChevronLeft, ChevronRight, ChevronDown, Copy, Check, X } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listServerAudit, getServerAuditStatus, setServerAuditForce, ServerAuditEvent, ServerAuditState } from '@/lib/api/serverAudit';
import { Skeleton, SkeletonText, SkeletonList } from '@/components/Skeleton';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';
import { auditActor, auditSentence } from '@/lib/auditSentences';
import { auditStep } from '@/lib/auditPaging';
import { timeAgo } from '@/lib/time';

// One page of the trail. The popup pages past it by asking for the next one.
const PAGE_SIZE = 50;

const EVENT_FILTERS: { id: string; label: string }[] = [
    { id: '',                      label: 'All events' },
    { id: 'power_action',          label: 'Power' },
    { id: 'member_invited',        label: 'Invites' },
    { id: 'member_removed',        label: 'Removals' },
    { id: 'member_perms_changed',  label: 'Permissions' },
    { id: 'resources_changed',     label: 'Resources' },
    { id: 'name_changed',          label: 'Name' },
];

export default function ServerAuditView() {
    const paramId = useRouteId('servers');
    const serverId = Number(paramId);
    const { user, servers } = useAppData();
    const isAdmin = !!user?.isAdmin;
    const server = servers.find(s => s.id === serverId);
    const isOwner = server?.ownerId === user?.id;

    const [state, setState] = useState<ServerAuditState | null>(null);
    const [events, setEvents] = useState<ServerAuditEvent[]>([]);
    const [total, setTotal] = useState(0);
    const [filter, setFilter] = useState<string>('');
    const [loading, setLoading] = useState(true);
    const [toggling, setToggling] = useState(false);

    const [loadingMore, setLoadingMore] = useState(false);
    const [loadError, setLoadError] = useState('');
    const [openIndex, setOpenIndex] = useState<number | null>(null);
    // Answers for a filter the user has already left must not land on the list.
    const reqSeq = useRef(0);
    const [slideDir, setSlideDir] = useState<-1 | 0 | 1>(0);

    const reload = useCallback(async () => {
        if (!Number.isFinite(serverId) || serverId <= 0) return;
        const seq = ++reqSeq.current;
        setOpenIndex(null);
        const [s, e] = await Promise.all([
            getServerAuditStatus(serverId),
            listServerAudit(serverId, { eventType: filter || undefined, limit: PAGE_SIZE, offset: 0 }),
        ]);
        if (seq !== reqSeq.current) return;
        if (s.success && s.state) setState(s.state);
        if (e.success) {
            setEvents(e.events || []);
            setTotal(e.total || 0);
            setLoadError('');
        } else {
            setLoadError(e.message || 'Could not load the audit log.');
        }
        setLoading(false);
    }, [serverId, filter]);

    // Resolves to the list length after the page landed, so the popup can step
    // onto the first new entry.
    const loadMore = useCallback(async (): Promise<number> => {
        const seq = reqSeq.current;
        setLoadingMore(true);
        const e = await listServerAudit(serverId, { eventType: filter || undefined, limit: PAGE_SIZE, offset: events.length });
        setLoadingMore(false);
        if (seq !== reqSeq.current) return events.length;
        if (!e.success) {
            setLoadError(e.message || 'Could not load more entries.');
            return events.length;
        }
        const page: ServerAuditEvent[] = e.events || [];
        // Rows written since the first page shift the offsets; skip what we hold.
        const seen = new Set(events.map(x => x.id));
        const merged = [...events, ...page.filter(x => !seen.has(x.id))];
        setEvents(merged);
        setTotal(e.total || 0);
        setLoadError('');
        return merged.length;
    }, [serverId, filter, events]);

    useEffect(() => { reload(); }, [reload]);

    const step = async (delta: -1 | 1) => {
        if (openIndex === null) return;
        const s = auditStep(openIndex, delta, events.length, total);
        if (s.kind === 'index') {
            setSlideDir(delta);
            setOpenIndex(s.index);
        } else if (s.kind === 'load' && !loadingMore) {
            const loaded = await loadMore();
            if (loaded > openIndex + 1) {
                setSlideDir(1);
                setOpenIndex(openIndex + 1);
            }
        }
    };

    const handleToggleForce = async () => {
        if (!state) return;
        setToggling(true);
        const res = await setServerAuditForce(serverId, !state.forceOn);
        setToggling(false);
        if (res.success) {
            setState(prev => prev ? { ...prev, forceOn: res.forceOn, effectiveOn: prev.enabled || res.forceOn } : prev);
        }
    };

    if (loading) return (
        <main className="flex-1 flex flex-col overflow-hidden p-6 gap-4">
            <header className="shrink-0 flex items-start justify-between gap-4">
                <div className="space-y-2">
                    <SkeletonText width="w-40" className="h-5" />
                    <SkeletonText width="w-80" className="h-3" />
                </div>
                <Skeleton className="w-24 h-7 rounded-md" />
            </header>
            <section className="card p-4 border border-(--base-03) flex items-center justify-between gap-3">
                <SkeletonText width="w-64" className="h-3" />
                <Skeleton className="w-28 h-7 rounded-md" />
            </section>
            <div className="shrink-0 flex items-center gap-1.5">
                {Array.from({ length: 7 }).map((_, i) => (
                    <Skeleton key={i} className="w-20 h-7 rounded-md" />
                ))}
            </div>
            <div className="flex-1 max-w-4xl">
                <SkeletonList rows={6} />
            </div>
        </main>
    );

    if (!isOwner && !isAdmin) {
        return (
            <main className="flex-1 flex items-center justify-center text-(--base-06) p-6">
                <p className="text-sm">The audit log is reserved for the server&apos;s owner and platform admins.</p>
            </main>
        );
    }

    return (
        <main className="flex-1 flex flex-col overflow-hidden p-6 gap-4">
            <header className="shrink-0 flex items-start justify-between gap-4">
                <div>
                    <h1 className="h-page flex items-center gap-2">
                        <FileText size={22} className="text-(--accent-light)" />
                        Audit log
                    </h1>
                    <p className="text-sm text-(--base-06) mt-1">
                        Who did what on this server. Audit only kicks in the first time you invite a member — solo-owner
                        servers don&apos;t accumulate audit overhead until they need to.
                    </p>
                </div>
                {state && (
                    <div className={`flex items-center gap-2 px-3 py-1.5 rounded-md border text-xs font-mono ${
                        state.effectiveOn
                            ? 'border-(--success-border) bg-(--success-ghost) text-(--success-light)'
                            : 'border-(--base-04) bg-(--base-03) text-(--base-06)'
                    }`}>
                        {state.effectiveOn ? <ShieldCheck size={14} /> : <ShieldOff size={14} />}
                        {state.effectiveOn ? 'ACTIVE' : 'DORMANT'}
                    </div>
                )}
            </header>

            {/* State summary */}
            {state && (
                <section className="card p-4 border border-(--base-03) flex flex-col sm:flex-row sm:items-center justify-between gap-3">
                    <div className="text-sm text-(--base-07)">
                        <p>
                            {state.enabled
                                ? <>Auto-enabled because a member was invited. </>
                                : <>Auto-enable hasn&apos;t kicked in yet. </>}
                            {state.forceOn
                                ? <>Admin has <span className="font-mono text-(--accent-light)">forced</span> audit on. </>
                                : null}
                            Logged events so far: <span className="font-mono">{state.eventCount}</span>.
                        </p>
                    </div>
                    {isAdmin && (
                        <button
                            type="button"
                            onClick={handleToggleForce}
                            disabled={toggling}
                            className={`btn btn-sm inline-flex items-center gap-2 disabled:opacity-40 ${
                                state.forceOn ? 'btn-secondary' : 'btn-primary'
                            }`}
                        >
                            {toggling && <Loader2 size={12} className="animate-spin" />}
                            {state.forceOn ? 'Drop force-on' : 'Force audit on'}
                        </button>
                    )}
                </section>
            )}

            {/* Filters */}
            <div className="shrink-0 flex items-center gap-1.5 overflow-x-auto">
                <Filter size={14} className="text-(--base-06) shrink-0" />
                {EVENT_FILTERS.map(f => (
                    <button
                        key={f.id || 'all'}
                        type="button"
                        onClick={() => setFilter(f.id)}
                        className={`px-3 py-1 rounded-md text-xs font-medium border whitespace-nowrap transition-colors ${
                            filter === f.id
                                ? 'border-(--accent-border) bg-(--accent-ghost) text-(--accent-light)'
                                : 'border-(--base-04) text-(--base-07) hover:bg-(--base-03)'
                        }`}
                    >
                        {f.label}
                    </button>
                ))}
            </div>

            {/* Events */}
            <div className="flex-1 overflow-y-auto">
                {loadError && (
                    <p role="alert" className="max-w-4xl mb-3 text-sm text-(--error-light) flex items-center gap-2">
                        <AlertTriangle size={14} className="shrink-0" />
                        {loadError}
                    </p>
                )}
                {events.length === 0 ? (
                    !loadError && <EmptyState effectiveOn={state?.effectiveOn ?? false} />
                ) : (
                    <ul className="space-y-1.5 max-w-4xl">
                        {events.map((ev, i) => (
                            <EventRow key={ev.id} ev={ev} onOpen={() => { setSlideDir(0); setOpenIndex(i); }} />
                        ))}
                    </ul>
                )}
                {events.length > 0 && events.length < total && (
                    <div className="max-w-4xl flex flex-col items-center gap-2 mt-4">
                        <p className="text-xs text-(--base-06)">Showing {events.length} of {total}</p>
                        <button type="button" onClick={() => { void loadMore(); }} disabled={loadingMore}
                            className="btn btn-secondary btn-sm inline-flex items-center gap-2 disabled:opacity-40">
                            {loadingMore && <Loader2 size={12} className="animate-spin" />}
                            Load more
                        </button>
                    </div>
                )}
            </div>

            {openIndex !== null && events[openIndex] && (
                <div className="modal-overlay animate-fade-in" onClick={() => setOpenIndex(null)}>
                    <AuditDetail
                        ev={events[openIndex]}
                        position={openIndex + 1}
                        total={Math.max(total, events.length)}
                        canPrev={openIndex > 0}
                        canNext={auditStep(openIndex, 1, events.length, total).kind !== 'none'}
                        busy={loadingMore}
                        slideDir={slideDir}
                        onStep={step}
                        onClose={() => setOpenIndex(null)}
                    />
                </div>
            )}
        </main>
    );
}

function absoluteTime(iso: string): string {
    return new Date(iso).toLocaleString();
}

function EventRow({ ev, onOpen }: { ev: ServerAuditEvent; onOpen: () => void }) {
    const actor = auditActor(ev);
    return (
        <li>
            <button
                type="button"
                onClick={onOpen}
                className="card w-full text-left px-3 py-2.5 flex items-center gap-3 hover:bg-(--base-03) hover:border-(--base-04) active:bg-(--base-04) transition-colors"
            >
                <span aria-hidden="true"
                    className="w-7 h-7 shrink-0 rounded-full bg-(--accent-ghost) text-(--accent-light) text-xs font-semibold flex items-center justify-center">
                    {actor.charAt(0).toUpperCase()}
                </span>
                <span className="flex-1 min-w-0 text-sm text-(--base-09) truncate">{auditSentence(ev)}</span>
                <time dateTime={ev.createdAt} title={absoluteTime(ev.createdAt)}
                    className="text-xs text-(--base-06) shrink-0 tabular-nums">
                    {timeAgo(ev.createdAt)}
                </time>
            </button>
        </li>
    );
}

function valueText(v: unknown): string {
    if (v === null || v === undefined) return 'none';
    if (Array.isArray(v)) return v.length === 0 ? 'none' : v.map(x => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ');
    if (typeof v === 'object') return JSON.stringify(v);
    return String(v);
}

function AuditDetail({ ev, position, total, canPrev, canNext, busy, slideDir, onStep, onClose }: {
    ev: ServerAuditEvent;
    position: number;
    total: number;
    canPrev: boolean;
    canNext: boolean;
    busy: boolean;
    slideDir: -1 | 0 | 1;
    onStep: (delta: -1 | 1) => void;
    onClose: () => void;
}) {
    const bodyRef = useRef<HTMLDivElement>(null);
    const [copied, setCopied] = useState<'idle' | 'copied' | 'failed'>('idle');
    const raw = JSON.stringify(ev, null, 2);
    const meta = Object.entries(ev.metadata ?? {});

    const stepRef = useRef({ onStep, canPrev, canNext, busy });
    useEffect(() => { stepRef.current = { onStep, canPrev, canNext, busy }; });

    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
            if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
            const t = e.target as HTMLElement | null;
            if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
            const s = stepRef.current;
            if (s.busy) return;
            if (e.key === 'ArrowLeft' && s.canPrev) { e.preventDefault(); s.onStep(-1); }
            if (e.key === 'ArrowRight' && s.canNext) { e.preventDefault(); s.onStep(1); }
        };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, []);

    // A short slide in the direction of travel, so paging reads as moving
    // along the list. Skipped on open (the overlay fades in) and when the
    // viewer asked for less motion.
    useEffect(() => {
        setCopied('idle');
        const el = bodyRef.current;
        if (!el || slideDir === 0 || typeof el.animate !== 'function') return;
        if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
        el.animate(
            [{ opacity: 0, transform: `translateX(${slideDir * 16}px)` }, { opacity: 1, transform: 'translateX(0)' }],
            { duration: 180, easing: 'cubic-bezier(0.2, 0, 0, 1)' },
        );
    }, [ev.id, slideDir]);

    const copy = async () => {
        try {
            await navigator.clipboard.writeText(raw);
            setCopied('copied');
        } catch {
            setCopied('failed');
        }
    };

    return (
        <ModalPanel onClose={onClose} className="modal-panel w-[calc(100%-32px)] max-w-2xl max-h-[85vh] flex flex-col">
            <div className="modal-header flex items-start justify-between gap-3">
                <h3 className="modal-title break-words min-w-0">{auditSentence(ev)}</h3>
                <button type="button" onClick={onClose} aria-label="Close" className="btn btn-ghost btn-icon btn-sm shrink-0">
                    <X size={14} />
                </button>
            </div>
            <div ref={bodyRef} className="modal-body overflow-y-auto space-y-4">
                <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
                    <dt className="text-(--base-06)">Actor</dt>
                    <dd className="text-(--base-09) break-words">{auditActor(ev)}</dd>
                    {ev.targetName && <>
                        <dt className="text-(--base-06)">Target</dt>
                        <dd className="text-(--base-09) break-words">{ev.targetName}</dd>
                    </>}
                    <dt className="text-(--base-06)">Time</dt>
                    <dd className="text-(--base-09)">
                        <time dateTime={ev.createdAt}>{absoluteTime(ev.createdAt)}</time>
                        <span className="text-(--base-06)"> ({timeAgo(ev.createdAt)})</span>
                    </dd>
                    {ev.ipAddress && <>
                        <dt className="text-(--base-06)">IP address</dt>
                        <dd className="text-(--base-09) font-mono break-all">{ev.ipAddress}</dd>
                    </>}
                    {ev.userAgent && <>
                        <dt className="text-(--base-06)">User agent</dt>
                        <dd className="text-(--base-09) break-words">{ev.userAgent}</dd>
                    </>}
                    <dt className="text-(--base-06)">Event</dt>
                    <dd className="text-(--base-09) font-mono break-all">{ev.eventType}</dd>
                </dl>

                {meta.length > 0 && (
                    <div className="rounded-lg border border-(--base-03) overflow-hidden">
                        <table className="w-full text-sm">
                            <caption className="sr-only">Details</caption>
                            <tbody>
                                {meta.map(([k, v]) => (
                                    <tr key={k} className="border-b border-(--base-03) last:border-b-0">
                                        <th scope="row" className="text-left font-normal text-(--base-06) px-3 py-1.5 align-top whitespace-nowrap">{k}</th>
                                        <td className="text-(--base-09) px-3 py-1.5 font-mono text-xs break-all">{valueText(v)}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                )}

                <details className="group">
                    <summary className="cursor-pointer select-none text-sm text-(--base-07) hover:text-(--base-09) inline-flex items-center gap-1.5 list-none [&::-webkit-details-marker]:hidden">
                        <ChevronDown size={14} className="-rotate-90 group-open:rotate-0 motion-safe:transition-transform" />
                        Raw JSON
                    </summary>
                    <div className="mt-2 relative">
                        <button type="button" onClick={copy} className="btn btn-secondary btn-sm absolute top-2 right-2 inline-flex items-center gap-1.5">
                            {copied === 'copied' ? <Check size={12} /> : <Copy size={12} />}
                            {copied === 'copied' ? 'Copied' : copied === 'failed' ? 'Copy failed' : 'Copy'}
                        </button>
                        <pre className="text-xs font-mono text-(--base-07) bg-(--base-01) border border-(--base-03) rounded-lg p-3 pr-24 overflow-x-auto">{raw}</pre>
                    </div>
                </details>
            </div>
            <div className="modal-footer justify-between! items-center">
                <button type="button" onClick={() => onStep(-1)} disabled={!canPrev || busy}
                    className="btn btn-secondary btn-sm inline-flex items-center gap-1 disabled:opacity-40">
                    <ChevronLeft size={14} /> Previous
                </button>
                <span className="text-xs text-(--base-06) tabular-nums inline-flex items-center gap-2" aria-live="polite">
                    {busy && <Loader2 size={12} className="animate-spin" />}
                    {position} of {total}
                </span>
                <button type="button" onClick={() => onStep(1)} disabled={!canNext || busy}
                    className="btn btn-secondary btn-sm inline-flex items-center gap-1 disabled:opacity-40">
                    Next <ChevronRight size={14} />
                </button>
            </div>
        </ModalPanel>
    );
}

function EmptyState({ effectiveOn }: { effectiveOn: boolean }) {
    return (
        <div className="text-center py-16 text-(--base-06) max-w-md mx-auto">
            <AlertTriangle size={28} className="mx-auto" />
            <p className="mt-3 text-sm">
                {effectiveOn
                    ? 'Audit is active but no events have been recorded yet for the selected filter.'
                    : 'Audit is dormant — it auto-activates the first time you invite a member. Solo-owner activity isn’t logged to save space.'}
            </p>
        </div>
    );
}
