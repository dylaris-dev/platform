"use client";

import { useState, useEffect, useCallback } from 'react';
import ModalPanel from '@/components/ui/ModalPanel';
import { Sparkles, X, AlertTriangle, RefreshCw, ChevronDown } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    getUpdates, markUpdatesSeen,
    type Release, type UpdateComponent, type UpdateRequirement,
} from '@/lib/api/updates';
import {
    groupInstances, serviceLabel, anythingOutdated, bellState,
    formatDeadline, mergeReleases, releasesSince, mandatoryApplies,
    type VersionGroup, type MergedReleases,
} from '@/lib/updateGroups';

// ---------------------------------------------------------------------------
// The updates view. Two questions, in this order:
//
//   1. Which of MY components are behind, and by how much
//   2. What changed
//
// The first is why anyone opens this, so it sits at the top. It used to be
// absent entirely: the old view listed changelog entries and left the reader to
// work out whether any of it applied to their deployment.
//
// Everyone sees it, not just admins. An admin runs the platform and sees the
// platform notes plus every component; a BYON customer sees the customer notes
// plus their own nodes, and used to see nothing at all - despite being the one
// reader with hardware to update.
// ---------------------------------------------------------------------------

// How often the view refreshes itself while open or mounted. Six hours, because
// releases are a human-paced event and Core caches the notes for fifteen minutes
// anyway; the button next to it exists for the case where six hours is too long.
const REFRESH_MS = 6 * 60 * 60 * 1000;

// Fixes are orange rather than green. Green reads as "nothing to see", and a
// fix is the category most likely to be the reason somebody opened this.
// Breaking additionally gets a box and a "please read", because it is the only
// category that can cost the reader something if they scroll past it.
const CATEGORY_STYLES: Record<string, { color: string; dot: string }> = {
    breaking: { color: 'text-(--error-light)', dot: 'bg-(--error)' },
    security: { color: 'text-(--error-light)', dot: 'bg-(--error-light)' },
    features: { color: 'text-(--accent-light)', dot: 'bg-(--accent)' },
    fixes: { color: 'text-(--warning-light)', dot: 'bg-(--warning)' },
};

// One version row inside a component: "2/3 on 2026.08.28".
//
// The count is written as a fraction rather than a bare number because "1 on
// 2026.08.20" does not say whether that is all of them or one straggler, and
// the straggler is the whole reason to look.
//
// It is omitted entirely where the fraction would lie. The panel reports itself
// through the browser that loaded it, so Core sees exactly one copy however many
// replicas are running, and "1/1" would read as "all of them are current".
function VersionRow({ group, countable }: { group: VersionGroup; countable: boolean }) {
    const unknown = group.version === '';
    return (
        <div className="flex items-baseline gap-2 text-[12px] leading-relaxed">
            {countable && (
                <span className="font-mono text-(--base-06) tabular-nums shrink-0">
                    {group.count}/{group.total}
                </span>
            )}
            <span
                className={`font-mono ${
                    group.outdated ? 'text-(--warning-light)' : unknown ? 'text-(--base-05)' : 'text-(--success-light)'
                }`}
                title={group.labels.join(', ')}
            >
                {unknown ? 'not reporting' : group.version}
            </span>
            {group.outdated && (
                <span className="text-[10px] font-mono uppercase tracking-[0.06em] text-(--warning-light)">
                    update available
                </span>
            )}
        </div>
    );
}

// One component: what it should be on, and what its instances actually are.
function ComponentRow({ component }: { component: UpdateComponent }) {
    const groups = groupInstances(component.instances);
    return (
        <div className="flex items-start gap-4 px-4 py-2.5 border-b border-(--base-03)/60 last:border-b-0">
            <div className="w-28 shrink-0">
                <div className="text-sm text-(--base-09)">
                    {component.label || serviceLabel(component.service)}
                </div>
                <div
                    className={`text-[10px] font-mono ${
                        !component.latest
                            ? 'text-(--base-05)'
                            : component.outdated
                              ? 'text-(--warning-light)'
                              : 'text-(--success-light)'
                    }`}
                >
                    {component.latest ? `latest ${component.latest}` : 'no releases'}
                </div>
            </div>
            <div className="min-w-0 flex-1 space-y-0.5">
                {groups.length === 0 ? (
                    // A component the releases name but nothing reports. Shown
                    // rather than hidden: a component the operator never updates
                    // and is never told about is exactly the case this exists for.
                    <div className="text-[12px] text-(--base-05)">Nothing reporting this component.</div>
                ) : (
                    groups.map(g => (
                        <VersionRow key={g.version || 'unknown'} group={g} countable={component.countable} />
                    ))
                )}
            </div>
        </div>
    );
}

// A mandatory update. Loud, and louder once the deadline has passed, because at
// that point the component is being refused rather than warned.
function RequirementBanner({ req }: { req: UpdateRequirement }) {
    return (
        <div className={`mx-4 mt-4 px-3 py-2.5 rounded-md border ${
            req.passed
                ? 'bg-(--error-ghost) border-(--error)/50'
                : 'bg-(--warning-ghost) border-(--warning)/40'
        }`}>
            <div className="flex items-start gap-2.5">
                <AlertTriangle
                    size={15}
                    className={`shrink-0 mt-0.5 ${req.passed ? 'text-(--error-light)' : 'text-(--warning-light)'}`}
                />
                <div className="min-w-0 text-[12px] leading-relaxed">
                    <div className={`font-medium ${req.passed ? 'text-(--error-light)' : 'text-(--warning-light)'}`}>
                        {req.passed
                            ? `${serviceLabel(req.service)} must be updated to ${req.minVersion} - the deadline has passed`
                            : `${serviceLabel(req.service)} must be updated to ${req.minVersion} by ${formatDeadline(req.deadline)}`}
                    </div>
                    {req.note && <div className="text-(--base-08) mt-0.5">{req.note}</div>}
                </div>
            </div>
        </div>
    );
}

// What is new since the reader last looked, as ONE block - a summary first, the
// entries only if they ask.
//
// Two things were wrong with showing it all. It rendered every release Core sent
// rather than the unacknowledged ones (see releasesSince), so a fully-current
// operator opened it to a span reaching back days. And even correctly filtered,
// a busy day is four releases, sixteen category headings and a dozen "Nothing
// this time." lines between the reader and the two entries that matter.
//
// So the default is the shape of the news - how many releases, how much of what
// kind - and the entries are one click away. Nobody opens a changelog to read
// it end to end; they open it to find out whether anything concerns them.
function MergedBlock({ merged, mandatory, expanded, onToggle }: {
    merged: MergedReleases;
    mandatory: boolean;
    expanded: boolean;
    onToggle: () => void;
}) {
    const filled = merged.categories.filter(c => c.entries.length > 0);
    const total = filled.reduce((n, c) => n + c.entries.length, 0);

    return (
        <section className="px-4 pt-4 pb-2">
            <div className="flex flex-wrap items-center gap-2 pb-2">
                <h3 className="text-sm font-display font-semibold text-(--base-09) font-mono">{merged.range}</h3>
                {merged.count > 1 && (
                    <span className="badge badge-neutral">{merged.count} releases</span>
                )}
                {/* Earned by a component of the reader's being below a floor, not
                    by a deadline existing somewhere in the window. */}
                {mandatory && <span className="badge badge-error">mandatory</span>}
            </div>

            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[12px]">
                {filled.length === 0 ? (
                    <span className="text-(--base-06)">No entries in these releases.</span>
                ) : (
                    filled.map(cat => (
                        <span key={cat.key} className="inline-flex items-center gap-1.5">
                            <span className={`font-mono text-[11px] font-semibold uppercase tracking-[0.06em] ${CATEGORY_STYLES[cat.key].color}`}>
                                {cat.label}
                            </span>
                            <span className="text-(--base-07) tabular-nums">{cat.entries.length}</span>
                        </span>
                    ))
                )}
            </div>

            {total > 0 && (
                <button
                    type="button"
                    onClick={onToggle}
                    aria-expanded={expanded}
                    className="mt-2 inline-flex items-center gap-1 text-[12px] text-(--base-07) hover:text-(--accent-light) transition-colors"
                >
                    <ChevronDown
                        size={13}
                        className={`transition-transform ${expanded ? 'rotate-180' : ''}`}
                    />
                    {expanded ? 'Hide details' : `Show details (${total})`}
                </button>
            )}

            {expanded && merged.categories.map(cat => {
                const style = CATEGORY_STYLES[cat.key];
                const isBreaking = cat.key === 'breaking';
                // An empty Breaking section gets no box: a highlighted frame
                // around "Nothing this time." shouts about the absence of news.
                const framed = isBreaking && cat.entries.length > 0;
                if (cat.entries.length === 0) return null;
                return (
                    <div
                        key={cat.key}
                        className={`mt-3 ${framed ? 'rounded-md border border-(--error)/40 bg-(--error)/5 p-3' : ''}`}
                    >
                        <div className="flex items-center gap-2">
                            <div className={`font-mono text-[11px] font-semibold uppercase tracking-[0.06em] ${style.color}`}>
                                {cat.label}
                            </div>
                            {framed && (
                                <span className="badge badge-error text-[10px]">please read</span>
                            )}
                        </div>
                        <ul className="mt-1.5 space-y-1.5">
                            {cat.entries.map((e, i) => (
                                <li key={i} className="text-[12px] leading-relaxed text-(--base-08)">
                                    {e.text}
                                    {(e.services?.length ?? 0) > 0 && (
                                        <span className="ml-1.5 inline-flex flex-wrap gap-1 align-middle">
                                            {e.services!.map(svc => (
                                                <code key={svc} className="badge badge-neutral text-[10px]">{svc}</code>
                                            ))}
                                        </span>
                                    )}
                                </li>
                            ))}
                        </ul>
                    </div>
                );
            })}
        </section>
    );
}

export default function UpdatesBell() {
    const { user } = useAppData();

    const [open, setOpen] = useState(false);
    const [loading, setLoading] = useState(false);
    const [components, setComponents] = useState<UpdateComponent[]>([]);
    const [releases, setReleases] = useState<Release[]>([]);
    const [required, setRequired] = useState<UpdateRequirement[]>([]);
    const [latest, setLatest] = useState<string | undefined>();
    const [seen, setSeen] = useState<string | undefined>();
    // What had been acknowledged when the modal was OPENED, not what is
    // acknowledged now. Opening marks everything seen, so reading the live value
    // here would empty the list the click was meant to show.
    const [seenAtOpen, setSeenAtOpen] = useState<string | undefined>();
    const [expanded, setExpanded] = useState(false);

    const refresh = useCallback(async () => {
        setLoading(true);
        const res = await getUpdates();
        setLoading(false);
        if (!res.success) return;
        setComponents(res.components ?? []);
        setReleases(res.releases ?? []);
        setRequired(res.required ?? []);
        setLatest(res.latest);
        setSeen(res.seen);
    }, []);

    useEffect(() => {
        if (!user) return;
        refresh();
        const t = setInterval(refresh, REFRESH_MS);
        return () => clearInterval(t);
    }, [user, refresh]);

    if (!user) return null;

    const outdated = anythingOutdated(components);
    const state = bellState({ outdated, required: required.length > 0, latest, seen });

    // Opening acknowledges the newest release so the "unseen" badge clears. It
    // deliberately does NOT clear the attention state: acknowledging that a
    // release exists is not the same as having updated, and a button that goes
    // quiet when you look at it stops reporting the thing it is for.
    const openModal = () => {
        setSeenAtOpen(seen);
        setExpanded(false);
        setOpen(true);
        if (latest && latest !== seen) {
            setSeen(latest);
            markUpdatesSeen().catch(() => {});
        }
    };

    const buttonClass =
        state === 'attention'
            ? 'text-(--warning-light)'
            : state === 'unseen'
                ? 'text-(--accent-light)'
                : '';

    const title =
        state === 'attention'
            ? 'Updates - something you run is behind'
            : state === 'unseen'
                ? 'Updates - a new release was published'
                : 'Updates - you are up to date';

    return (
        <>
            <button
                type="button"
                onClick={openModal}
                title={title}
                aria-label={title}
                className={`app-icon-btn relative ${buttonClass}`}
            >
                <Sparkles size={16} />
                {state !== 'idle' && (
                    <span className={`absolute -top-1 -right-1 w-[10px] h-[10px] rounded-full border-2 border-(--base-01) ${
                        state === 'attention' ? 'bg-(--warning)' : 'bg-(--accent-light)'
                    }`} />
                )}
            </button>

            {open && (
                <div
                    className="modal-overlay animate-fade-in z-50"
                    onClick={e => { if (e.target === e.currentTarget) setOpen(false); }}
                >
                    <ModalPanel onClose={() => setOpen(false)} label="Updates" className="modal-panel w-full max-w-3xl">
                        <div className="flex items-center justify-between px-4 py-3 border-b border-(--base-03)">
                            <div className="flex items-center gap-2">
                                <Sparkles size={15} className="text-(--accent-light)" />
                                <span className="text-sm font-display font-semibold text-(--base-09)">Updates</span>
                                {latest && (
                                    <span className="font-mono text-[10px] text-(--success-light)">latest {latest}</span>
                                )}
                            </div>
                            <div className="flex items-center gap-1">
                                <button
                                    type="button"
                                    onClick={refresh}
                                    disabled={loading}
                                    className="text-(--base-06) hover:text-(--base-09) p-1 rounded-md hover:bg-(--base-03) transition-colors disabled:opacity-50"
                                    aria-label="Check again"
                                    title="Check again"
                                >
                                    <RefreshCw size={15} className={loading ? 'animate-spin' : ''} />
                                </button>
                                <button
                                    type="button"
                                    onClick={() => setOpen(false)}
                                    className="text-(--base-06) hover:text-(--base-09) p-1 rounded-md hover:bg-(--base-03) transition-colors"
                                    aria-label="Close"
                                >
                                    <X size={16} />
                                </button>
                            </div>
                        </div>

                        <div className="max-h-[75vh] overflow-y-auto pb-4">
                            {required.map(req => <RequirementBanner key={req.service} req={req} />)}

                            {components.length > 0 && (
                                <>
                                    <p className="mt-4 mx-4 text-[11px] leading-relaxed text-(--base-06)">
                                        <span className="text-(--success-light)">Green</span> means nothing is
                                        waiting for that component.{' '}
                                        <span className="text-(--warning-light)">Orange</span> means a release
                                        after the build you run named it. A release that only changed another
                                        component leaves this one green.
                                    </p>
                                    <div className="mt-2 mx-4 rounded-md border border-(--base-03) bg-(--base-02)">
                                        {components.map(c => <ComponentRow key={c.key} component={c} />)}
                                    </div>
                                </>
                            )}

                            {(() => {
                                if (releases.length === 0) {
                                    return (
                                        <div className="px-4 py-10 text-sm text-(--base-06) text-center">
                                            No releases published yet.
                                        </div>
                                    );
                                }
                                const fresh = releasesSince(releases, seenAtOpen);
                                if (fresh.length === 0) {
                                    return (
                                        <div className="px-4 py-10 text-sm text-(--base-06) text-center">
                                            You have read everything published so far.
                                        </div>
                                    );
                                }
                                const merged = mergeReleases(fresh);
                                if (!merged) return null;
                                return (
                                    <MergedBlock
                                        merged={merged}
                                        mandatory={mandatoryApplies(required)}
                                        expanded={expanded}
                                        onToggle={() => setExpanded(v => !v)}
                                    />
                                );
                            })()}
                        </div>
                    </ModalPanel>
                </div>
            )}
        </>
    );
}
