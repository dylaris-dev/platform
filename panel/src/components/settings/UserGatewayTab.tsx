"use client";

import React, { useEffect, useState } from 'react';
import { CircleAlert, RefreshCw } from 'lucide-react';
import { Skeleton } from '@/components/Skeleton';
import { fmtDate, fmtDateTime } from '@/lib/time';
import { formatDecimalBytes } from '@/lib/format';
import { barPercent } from '@/lib/gatewayProvider';
import {
    getUserGateway, productLabel, limitText, kitState, routeSuspendedLabel, poolText, truncatedNote, EMPTY_TEXT,
    failureText, hiddenNote, NOT_FOR_ROLE, playersLabel, peakLine, peakSeries, peakDayList, PEAK_NO_DATA, type UserGateway,
} from '@/lib/userGateway';

// Read-only view of one managed gateway customer, as this platform (the
// provider) holds it. Loaded when the tab opens and on Refresh only: every
// load reads the account's links, routes, domains, traffic and keys.

function Section({ title, empty, truncated, shown, children }: {
    title: string; empty: string | null; truncated?: boolean; shown?: number; children?: React.ReactNode;
}) {
    const note = truncatedNote(!!truncated, shown ?? 0);
    return (
        <section>
            <h4 className="mono-label mb-2">{title}</h4>
            {empty !== null ? (
                <p className="text-xs text-(--base-06) rounded-md bg-(--base-02) border border-(--base-03) p-3">{empty}</p>
            ) : (
                <ul className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">{children}</ul>
            )}
            {note && <p className="text-xs text-(--warning-light) mt-1.5" role="status">{note}</p>}
        </section>
    );
}

/** A block Core left out for the caller's role: one muted line. */
function Muted({ title, text }: { title: string; text: string }) {
    return (
        <section>
            <h4 className="mono-label mb-2">{title}</h4>
            <p className="text-xs text-(--base-06)">{text}</p>
        </section>
    );
}

function Row({ main, sub, badge }: { main: React.ReactNode; sub?: React.ReactNode; badge?: React.ReactNode }) {
    return (
        <li className="flex items-start justify-between gap-3 px-3 py-2.5 bg-(--base-01)">
            <div className="min-w-0">
                <div className="text-sm text-(--base-09) font-mono truncate">{main}</div>
                {sub && <div className="text-xs text-(--base-06) mt-0.5">{sub}</div>}
            </div>
            {badge}
        </li>
    );
}

const TONE: Record<string, string> = { success: 'badge-success', neutral: 'badge-neutral', warning: 'badge-warning', error: 'badge-error' };

function Badge({ tone, children }: { tone: string; children: React.ReactNode }) {
    return <span className={`badge ${TONE[tone] ?? 'badge-neutral'} shrink-0`}>{children}</span>;
}

/** 30 daily bars, oldest first. A day without data is a dashed mark on the
 *  baseline, never a bar of 0: nothing was measured that day. */
function PeakBars({ series }: { series: (number | null)[] }) {
    const max = Math.max(1, ...series.map(v => v ?? 0));
    return (
        <div className="flex items-end gap-px h-8" role="img" aria-label="Daily peak of concurrent players, last 30 days">
            {series.map((v, i) => (
                v === null
                    ? <span key={i} className="flex-1 border-b border-dashed border-(--base-05)" title={PEAK_NO_DATA} />
                    : <span key={i} className="flex-1 rounded-t-sm bg-(--accent)" style={{ height: `${Math.max(6, (v / max) * 100)}%` }} title={String(v)} />
            ))}
        </div>
    );
}

/** The daily peaks: two lines, a 30-day chart of the totals and the days since the first row. */
function PlayerPeaks({ peaks }: { peaks: NonNullable<UserGateway['peaks']> }) {
    if (!peaks.days.length) {
        return <Section title="Player peaks" empty={EMPTY_TEXT.peaks} />;
    }
    const series = peakSeries(peaks.days, peaks.day);
    return (
        <section>
            <h4 className="mono-label mb-2">Player peaks</h4>
            <div className="rounded-md bg-(--base-02) border border-(--base-03) p-3 space-y-2 mb-2">
                <p className="text-xs text-(--base-09)">{peakLine('Peak today', peaks.today, true)}</p>
                <p className="text-xs text-(--base-09)">{peakLine('Peak in the last 30 days', peaks.last30Days, false)}</p>
                <PeakBars series={series} />
                <p className="text-xs text-(--base-06)">Sampled once a minute: each figure is at least that many. A dashed day has no data.</p>
            </div>
            <ul className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                {peakDayList(peaks.days, peaks.day).map(({ day, peak: d }) => (
                    <Row key={day} main={day} sub={d ? `${d.java} Java · ${d.bedrock} Bedrock` : PEAK_NO_DATA}
                        badge={<span className="text-sm font-mono text-(--base-08) shrink-0">{d ? d.total : '-'}</span>} />
                ))}
            </ul>
        </section>
    );
}

export default function UserGatewayTab({ userId }: { userId: string }) {
    const [data, setData] = useState<UserGateway | null>(null);
    const [error, setError] = useState('');
    // A 403: this role may not open the view at all, which is not a failure.
    const [denied, setDenied] = useState(false);
    const [loading, setLoading] = useState(true);
    const [slow, setSlow] = useState(false);

    // Bumped by Refresh; the effect owns every load, so a late answer for a
    // user no longer shown is dropped.
    const [nonce, setNonce] = useState(0);

    useEffect(() => {
        let live = true;
        const t = setTimeout(() => setSlow(true), 400);
        getUserGateway(userId).then(res => {
            if (!live) return;
            if (res.success) {
                setData(res.data);
                setError('');
            } else {
                setDenied(res.status === 403);
                setError(failureText(res));
            }
            setLoading(false);
            setSlow(false);
        });
        return () => { live = false; clearTimeout(t); };
    }, [userId, nonce]);

    const reload = () => {
        setLoading(true);
        setError('');
        setNonce(n => n + 1);
    };

    const refresh = (
        <button
            type="button"
            onClick={reload}
            disabled={loading}
            className="btn btn-secondary btn-sm shrink-0 disabled:opacity-40 inline-flex items-center gap-1.5"
            aria-label="Refresh the managed gateway view"
        >
            <RefreshCw size={13} className={loading ? 'animate-spin motion-reduce:animate-none' : ''} /> Refresh
        </button>
    );

    if (loading && !data) {
        return slow ? (
            <div className="space-y-3" aria-busy="true">
                <Skeleton className="h-5 w-40" />
                <Skeleton className="h-24 w-full" />
                <Skeleton className="h-24 w-full" />
            </div>
        ) : null;
    }
    if (denied && !data) {
        return <p className="text-xs text-(--base-06)">{NOT_FOR_ROLE}</p>;
    }
    if (error && !data) {
        return (
            <div className="space-y-3">
                <div className="alert alert-error text-xs" role="alert">
                    <CircleAlert size={14} className="shrink-0 mt-0.5" />
                    <span>{error}</span>
                </div>
                {refresh}
            </div>
        );
    }
    if (!data) return null;

    const { limits, usage, apiKeys } = data;
    const pool = usage?.pool;
    const pct = barPercent(pool?.percent);
    const poolLine = poolText(pool, formatDecimalBytes);

    return (
        <div className="space-y-5" aria-busy={loading}>
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0 space-y-1">
                    {data.product && data.billingStatus && (
                        <div className="flex flex-wrap items-center gap-2">
                            <span className="text-sm text-(--base-09) font-medium">{productLabel(data.product)}</span>
                            <Badge tone={data.billingStatus === 'active' ? 'success' : 'warning'}>{data.billingStatus}</Badge>
                        </div>
                    )}
                    {data.overLimitSince && (
                        <p className="text-xs text-(--warning-light)">Over its limits since {fmtDateTime(data.overLimitSince)}.</p>
                    )}
                </div>
                {refresh}
            </div>
            {error && (
                <div className="alert alert-error text-xs" role="alert">
                    <CircleAlert size={14} className="shrink-0 mt-0.5" />
                    <span>{error}</span>
                </div>
            )}

            {!limits ? (
                <Muted title="Plan and limits" text={hiddenNote(data.hidden, 'billing') ?? NOT_FOR_ROLE} />
            ) : (
                <section>
                    <h4 className="mono-label mb-2">Plan and limits</h4>
                    <dl className="grid grid-cols-3 gap-3 rounded-md bg-(--base-02) border border-(--base-03) p-3">
                        {([
                            ['Links', `${data.kitsUsed} of ${limitText(limits.maxLinks)}`],
                            // The address cap counts our domains only, so no "n of" here.
                            ['Address limit', limitText(limits.maxRoutes)],
                            ['Own-domain limit', limitText(limits.maxCustomDomains)],
                        ] as const).map(([k, v]) => (
                            <div key={k} className="min-w-0">
                                <dt className="mono-label">{k}</dt>
                                <dd className="text-sm text-(--base-09) mt-1">{v}</dd>
                            </div>
                        ))}
                    </dl>
                    {data.kitsRetiring > 0 && (
                        <p className="text-xs text-(--base-06) mt-1.5">{data.kitsRetiring} more being replaced, outside the limit.</p>
                    )}
                </section>
            )}

            <Section title="Links" empty={data.kits.length ? null : EMPTY_TEXT.kits} truncated={data.kitsTruncated} shown={data.kits.length}>
                {data.kits.map(k => {
                    const s = kitState(k);
                    const sub = [k.name, k.region || 'any region', `created ${fmtDate(k.createdAt)}`,
                        k.retireAt ? `removed ${fmtDateTime(k.retireAt)}` : '', k.caps.length ? k.caps.join(', ') : '']
                        .filter(Boolean).join(' · ');
                    return <Row key={k.linkId} main={k.linkId} sub={sub} badge={<Badge tone={s.tone}>{s.label}</Badge>} />;
                })}
            </Section>

            {data.players && (
                <p className="text-xs text-(--base-06)">
                    Players now: <span className="text-(--base-09)">{playersLabel(data.players)}</span>
                </p>
            )}

            <Section title="Addresses" empty={data.routes.length ? null : EMPTY_TEXT.routes} truncated={data.routesTruncated} shown={data.routes.length}>
                {data.routes.map(r => {
                    const suspended = routeSuspendedLabel(r);
                    const sub = [`${r.targetHost}:${r.targetPort}`, r.linkId || 'no link', r.proxyProtocol ? 'real IP on' : ''].filter(Boolean).join(' · ');
                    const badge = suspended
                        ? <Badge tone="error">{suspended}</Badge>
                        : r.players ? <span className="text-xs font-mono text-(--base-08) shrink-0">{playersLabel(r.players)}</span> : undefined;
                    return <Row key={r.domain} main={r.domain} sub={sub} badge={badge} />;
                })}
            </Section>

            {data.peaks && (
                <PlayerPeaks peaks={data.peaks} />
            )}

            <Section title="Own domains" empty={data.customDomains.length ? null : EMPTY_TEXT.customDomains} truncated={data.customDomainsTruncated} shown={data.customDomains.length}>
                {data.customDomains.map(c => (
                    <Row
                        key={c.domain}
                        main={c.domain}
                        sub={c.lapsesAt ? `Record missing, routes removed ${fmtDateTime(c.lapsesAt)}` : c.checkedAt ? `Checked ${fmtDateTime(c.checkedAt)}` : `${c.attempts} attempts`}
                        badge={<Badge tone={c.state === 'verified' ? (c.lapsesAt ? 'warning' : 'success') : 'neutral'}>{c.state}</Badge>}
                    />
                ))}
            </Section>

            {!usage ? (
                <Muted title="Traffic" text={hiddenNote(data.hidden, 'usage') ?? NOT_FOR_ROLE} />
            ) : (
                <section>
                    <h4 className="mono-label mb-2">Traffic {usage.period}</h4>
                    {pool && poolLine && (
                        <div className="space-y-1.5 mb-2">
                            <div
                                className="h-1.5 rounded-full bg-(--base-03) overflow-hidden"
                                role="progressbar"
                                aria-label="Traffic pool used"
                                aria-valuemin={0}
                                aria-valuemax={100}
                                aria-valuenow={pct}
                            >
                                <div className={`h-full rounded-full ${pct >= 90 ? 'bg-(--warning)' : 'bg-(--accent)'}`} style={{ width: `${pct}%` }} />
                            </div>
                            <p className={`text-xs ${pct >= 90 ? 'text-(--warning-light)' : 'text-(--base-06)'}`}>{poolLine}</p>
                        </div>
                    )}
                    {usage.regions.length ? (
                        <ul className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                            {usage.regions.map(g => (
                                <Row key={`${g.region}|${g.kind}|${g.product}`} main={g.region || 'unknown region'} sub={`${g.kind} · ${g.product}`}
                                    badge={<span className="text-sm font-mono text-(--base-08) shrink-0">{formatDecimalBytes(g.bytes)}</span>} />
                            ))}
                        </ul>
                    ) : (
                        <p className="text-xs text-(--base-06) rounded-md bg-(--base-02) border border-(--base-03) p-3">{EMPTY_TEXT.regions}</p>
                    )}
                </section>
            )}

            <Section title="Beam keys" empty={data.beamKeys.length ? null : EMPTY_TEXT.beamKeys} truncated={data.beamKeysTruncated} shown={data.beamKeys.length}>
                {data.beamKeys.map(k => (
                    <Row key={k.kid} main={k.kid} sub={`created ${fmtDate(k.createdAt)}`}
                        badge={k.revokedAt ? <Badge tone="neutral">Revoked</Badge> : <Badge tone="success">Active</Badge>} />
                ))}
            </Section>

            {!apiKeys ? (
                <Muted title="API keys" text={hiddenNote(data.hidden, 'apiKeys') ?? NOT_FOR_ROLE} />
            ) : (
                <Section title="API keys" empty={apiKeys.length ? null : EMPTY_TEXT.apiKeys} truncated={data.apiKeysTruncated} shown={apiKeys.length}>
                    {apiKeys.map(k => (
                        <Row key={k.id} main={k.name} sub={[k.permissions.join(', '), `created ${fmtDate(k.createdAt)}`,
                            k.lastUsedAt ? `last used ${fmtDateTime(k.lastUsedAt)}` : 'never used'].join(' · ')}
                            badge={k.revokedAt ? <Badge tone="neutral">Revoked</Badge> : undefined} />
                    ))}
                </Section>
            )}
        </div>
    );
}
