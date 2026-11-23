"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';

import { Loader2, ShieldCheck, ShieldOff, AlertTriangle, ChevronLeft, ChevronRight, ChevronDown, Copy, Check, X, ScrollText } from 'lucide-react';
import { Dialog, DialogBackdrop, DialogClose, DialogContent, DialogTitle } from '@ui-sdk/react/dialog';
import { useAppData } from '@/lib/AppDataContext';
import { systemEvents } from '@/lib/systemEvents';
import { listServerAudit, getServerAuditStatus, setServerAuditForce, ServerAuditEvent, ServerAuditState } from '@/lib/api/serverAudit';
import { Skeleton, SkeletonText, SkeletonList } from '@/components/Skeleton';
import { useRouteId } from '@/lib/routeParams';
import Select from '@/components/ui/Select';
import Callout from '@/components/server/Callout';
import { auditActor, auditSentence } from '@/lib/auditSentences';
import { auditStep } from '@/lib/auditPaging';
import { fmtDateTime, timeAgo } from '@/lib/time';

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

    // Core announces every row it writes. The newest page is fetched again and
    // only what is new goes on top, so pages already loaded and an open entry
    // stay where they are.
    const eventsRef = useRef(events);
    eventsRef.current = events;
    useEffect(() => {
        let timer: ReturnType<typeof setTimeout> | null = null;
        const refreshHead = async () => {
            const seq = reqSeq.current;
            const e = await listServerAudit(serverId, { eventType: filter || undefined, limit: PAGE_SIZE, offset: 0 });
            if (seq !== reqSeq.current || !e.success) return;
            const seen = new Set(eventsRef.current.map(x => x.id));
            const fresh = ((e.events || []) as ServerAuditEvent[]).filter(x => !seen.has(x.id));
            setTotal(e.total || 0);
            if (!fresh.length) return;
            setEvents(prev => [...fresh, ...prev]);
            setOpenIndex(i => i === null ? null : i + fresh.length);
        };
        const unsub = systemEvents.on('server_audit.changed', evt => {
            if ((evt.payload as { serverId?: number } | undefined)?.serverId !== serverId) return;
            // One action can write several rows; one fetch covers them.
            if (timer) clearTimeout(timer);
            timer = setTimeout(() => { timer = null; void refreshHead(); }, 400);
        });
        return () => { unsub(); if (timer) clearTimeout(timer); };
    }, [serverId, filter]);

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
        <main className="flex h-full flex-col gap-5 overflow-hidden">
            <header className="flex shrink-0 items-start justify-between gap-4">
                <div className="space-y-2">
                    <SkeletonText width="w-40" className="h-6" />
                    <SkeletonText width="w-80" className="h-3" />
                </div>
                <Skeleton className="h-6 w-24 rounded-full" />
            </header>
            <Skeleton className="h-16 w-full rounded-xl" />
            <Skeleton className="h-9 w-48 rounded-lg" />
            <div className="flex-1">
                <SkeletonList rows={6} />
            </div>
        </main>
    );

    if (!isOwner && !isAdmin) {
        return (
            <main className="flex h-full items-center justify-center text-(--base-06)">
                <p className="text-sm">The audit log is reserved for the server&apos;s owner and platform admins.</p>
            </main>
        );
    }

    return (
        <main className="flex h-full flex-col gap-5 overflow-hidden">
            <header className="flex shrink-0 flex-wrap items-start justify-between gap-x-6 gap-y-3">
                <div className="min-w-0">
                    <h1 className="font-display text-xl font-bold text-(--base-09)">Audit log</h1>
                    <p className="mt-1 max-w-2xl text-sm text-(--base-06)">
                        Who did what on this server. Audit only kicks in the first time you invite a member; solo-owner
                        servers don&apos;t accumulate audit overhead until they need to.
                    </p>
                </div>
                {state && (
                    <span className={`srv-chip h-7 gap-1.5 px-3 ${
                        state.effectiveOn ? 'bg-(--success-ghost) text-(--success-light)' : 'bg-(--base-03) text-(--base-06)'
                    }`}>
                        {state.effectiveOn ? <ShieldCheck size={14} aria-hidden="true" /> : <ShieldOff size={14} aria-hidden="true" />}
                        {state.effectiveOn ? 'Active' : 'Dormant'}
                    </span>
                )}
            </header>

            {/* State summary */}
            {state && (
                <section className="srv-surface flex shrink-0 flex-col justify-between gap-3 p-5 sm:flex-row sm:items-center">
                    <p className="text-sm text-(--base-07)">
                        {state.enabled
                            ? <>Auto-enabled because a member was invited. </>
                            : <>Auto-enable hasn&apos;t kicked in yet. </>}
                        {state.forceOn
                            ? <>Admin has <span className="font-medium text-(--accent-light)">forced</span> audit on. </>
                            : null}
                        Logged events so far: <span className="srv-num text-(--base-09)">{state.eventCount}</span>.
                    </p>
                    {isAdmin && (
                        <button
                            type="button"
                            onClick={handleToggleForce}
                            disabled={toggling}
                            className={`btn btn-sm inline-flex shrink-0 items-center gap-2 ${
                                state.forceOn ? 'btn-secondary' : 'btn-primary'
                            }`}
                        >
                            {toggling && <Loader2 size={14} className="animate-spin motion-reduce:animate-none" />}
                            {state.forceOn ? 'Drop force-on' : 'Force audit on'}
                        </button>
                    )}
                </section>
            )}

            {/* Filter */}
            <div className="flex shrink-0 items-center gap-3">
                <Select
                    ariaLabel="Filter by event type"
                    className="w-48"
                    value={filter}
                    onChange={setFilter}
                    options={EVENT_FILTERS.map(f => ({ value: f.id, label: f.label }))}
                />
                {total > 0 && (
                    <span className="srv-num text-xs text-(--base-06)">{total} {total === 1 ? 'event' : 'events'}</span>
                )}
            </div>

            {/* Events */}
            <div className="flex-1 overflow-y-auto">
                {loadError && (
                    <div className="mb-3">
                        <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{loadError}</Callout>
                    </div>
                )}
                {events.length === 0 ? (
                    !loadError && <EmptyState effectiveOn={state?.effectiveOn ?? false} />
                ) : (
                    <ul className="-mx-2">
                        {events.map((ev, i) => (
                            <EventRow key={ev.id} ev={ev} onOpen={() => { setSlideDir(0); setOpenIndex(i); }} />
                        ))}
                    </ul>
                )}
                {events.length > 0 && events.length < total && (
                    <div className="mt-4 flex flex-col items-center gap-2">
                        <p className="srv-num text-xs text-(--base-06)">Showing {events.length} of {total}</p>
                        <button type="button" onClick={() => { void loadMore(); }} disabled={loadingMore}
                            className="btn btn-secondary btn-sm inline-flex items-center gap-2">
                            {loadingMore && <Loader2 size={14} className="animate-spin motion-reduce:animate-none" />}
                            Load more
                        </button>
                    </div>
                )}
            </div>

            <Dialog open={openIndex !== null && !!events[openIndex]} onOpenChange={o => { if (!o) setOpenIndex(null); }}>
                <DialogBackdrop className="modal-overlay animate-fade-in" />
                <DialogContent className="modal-panel fixed inset-0 m-auto flex h-fit max-h-[85vh] w-[calc(100%-2rem)] max-w-2xl flex-col">
                    {openIndex !== null && events[openIndex] && (
                        <AuditDetail
                            ev={events[openIndex]}
                            position={openIndex + 1}
                            total={Math.max(total, events.length)}
                            canPrev={openIndex > 0}
                            canNext={auditStep(openIndex, 1, events.length, total).kind !== 'none'}
                            busy={loadingMore}
                            slideDir={slideDir}
                            onStep={step}
                        />
                    )}
                </DialogContent>
            </Dialog>
        </main>
    );
}

function absoluteTime(iso: string): string {
    return fmtDateTime(iso);
}

function EventRow({ ev, onOpen }: { ev: ServerAuditEvent; onOpen: () => void }) {
    const actor = auditActor(ev);
    return (
        <li>
            <button
                type="button"
                onClick={onOpen}
                className="flex w-full items-center gap-3 rounded-lg px-2 py-2.5 text-left transition-colors hover:bg-(--base-02) focus-visible:shadow-(--focus-ring) focus-visible:outline-none active:bg-(--base-03)"
            >
                <span aria-hidden="true"
                    className="flex size-7 shrink-0 items-center justify-center rounded-full bg-(--base-03) text-xs font-semibold text-(--base-07)">
                    {actor.charAt(0).toUpperCase()}
                </span>
                <span className="min-w-0 flex-1 truncate text-sm text-(--base-09)">{auditSentence(ev)}</span>
                <time dateTime={ev.createdAt} title={absoluteTime(ev.createdAt)}
                    className="srv-num shrink-0 text-xs text-(--base-06)">
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

function AuditDetail({ ev, position, total, canPrev, canNext, busy, slideDir, onStep }: {
    ev: ServerAuditEvent;
    position: number;
    total: number;
    canPrev: boolean;
    canNext: boolean;
    busy: boolean;
    slideDir: -1 | 0 | 1;
    onStep: (delta: -1 | 1) => void;
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
        <>
            <div className="modal-header flex items-start justify-between gap-3">
                <DialogTitle className="modal-title min-w-0 break-words">{auditSentence(ev)}</DialogTitle>
                <DialogClose aria-label="Close" className="btn btn-ghost btn-icon btn-sm shrink-0">
                    <X size={16} />
                </DialogClose>
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
                        <time dateTime={ev.createdAt} className="srv-num">{absoluteTime(ev.createdAt)}</time>
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
                    <div className="overflow-hidden rounded-lg bg-(--base-03)">
                        <table className="w-full text-sm">
                            <caption className="sr-only">Details</caption>
                            <tbody>
                                {meta.map(([k, v]) => (
                                    <tr key={k} className="border-b border-(--base-04) last:border-b-0">
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
                        <pre className="overflow-x-auto rounded-lg bg-(--base-01) p-3 pr-24 font-mono text-xs text-(--base-07)">{raw}</pre>
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
        </>
    );
}

function EmptyState({ effectiveOn }: { effectiveOn: boolean }) {
    return (
        <div className="srv-surface mx-auto flex max-w-md flex-col items-center gap-2 px-6 py-14 text-center">
            <ScrollText size={24} className="text-(--base-05)" aria-hidden="true" />
            <p className="text-sm text-(--base-07)">
                {effectiveOn
                    ? 'Audit is active but no events have been recorded yet for the selected filter.'
                    : 'Audit is dormant. It auto-activates the first time you invite a member. Solo-owner activity isn’t logged to save space.'}
            </p>
        </div>
    );
}
