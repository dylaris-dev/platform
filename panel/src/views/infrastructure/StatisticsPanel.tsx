"use client";

import { useCallback, useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid } from 'recharts';
import { BarChart3, ChevronDown, Download, Info, RefreshCw, Users } from 'lucide-react';
import { SkeletonCard, SkeletonStatGrid } from '@/components/Skeleton';
import { attachHeadlines, fleetTiles } from '@/lib/statisticsLayout';
import {
    downloadMetricsExport,
    formatMetric,
    getMetricsCatalog,
    getMetricsSeries,
    getMetricsSummary,
    pointValue,
    type MetricCoverage,
    type MetricHeadline,
    type MetricSeries,
    type MetricSeriesInfo,
    type MetricsUnavailableReason,
} from '@/lib/api/metrics';
import { useInfra } from './context';
import { liveStatus } from './livePlayers';
import { fmtDate, fmtDateTime, fmtTime } from '@/lib/time';

/**
 * The long-term record.
 *
 * Everything on this screen is a re-aggregation of stored buckets, so the same
 * question asked over an hour and over a year is one query with a different
 * step. What the page has to get right is which NUMBER a series means: a
 * counter is the total in its bucket, a gauge is the average across it, and
 * showing one as the other is not a rounding error - it is a different claim.
 */

const RANGES = [
    { id: '24h', label: '24 hours' },
    { id: '7d', label: '7 days' },
    { id: '30d', label: '30 days' },
    { id: '90d', label: '90 days' },
    { id: '365d', label: '12 months' },
] as const;

const tooltipStyle = {
    backgroundColor: 'var(--base-02)',
    border: '1px solid var(--base-04)',
    borderRadius: 'var(--radius-md)',
    fontSize: '12px',
    color: 'var(--base-09)',
    boxShadow: 'var(--shadow-md)',
    padding: '8px 12px',
};

/** Axis label density, by range. A day wants clock time, a year wants months. */
function formatTick(iso: string, range: string): string {
    const d = new Date(iso);
    if (range === '24h') return fmtTime(d, { hour: '2-digit', minute: '2-digit' });
    if (range === '365d') return fmtDate(d, { month: 'short', year: '2-digit' });
    return fmtDate(d, { day: '2-digit', month: 'short' });
}

function Unavailable({ reason, message }: { reason?: MetricsUnavailableReason; message?: string }) {
    return (
        <div className="card p-8 flex flex-col items-center text-center gap-3">
            <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center">
                <BarChart3 size={18} className="text-(--base-06)" />
            </div>
            <p className="text-sm text-(--base-08) max-w-md">
                {message ?? 'There is nothing recorded yet.'}
            </p>
            {reason === 'disabled' && (
                <p className="text-xs text-(--base-06) max-w-md">
                    Recording is off by default. Nothing that happened before it is switched on can be
                    recovered afterwards, so the sooner it is on, the further back the record goes.
                </p>
            )}
        </div>
    );
}

function howLabel(how: MetricHeadline['how']): string {
    return how === 'peak' ? 'Peak' : how === 'total' ? 'Total' : 'Average';
}

/**
 * A headline whose chart is in another group: one line, and a click away from
 * that chart. The numbers used to be a grid of cards of their own above the
 * charts; they now sit on the chart they summarise.
 */
function HeadlineChip({ h, onOpen }: { h: MetricHeadline; onOpen?: () => void }) {
    const body = (
        <>
            <span className="text-(--base-06)">{h.label}</span>
            <span className="font-mono tabular-nums text-(--base-09)">{formatMetric(h.value, h.unit)}</span>
        </>
    );
    return onOpen ? (
        <button
            type="button"
            onClick={onOpen}
            title={h.help ?? `${howLabel(h.how)} over the period. Opens its chart.`}
            className="inline-flex items-center gap-2 rounded-md border border-(--base-03) bg-(--base-02) px-2.5 py-1 text-xs transition-colors hover:border-(--base-05) focus-visible:outline-2 focus-visible:outline-(--accent)"
        >
            {body}
        </button>
    ) : (
        <span title={h.help} className="inline-flex items-center gap-2 rounded-md border border-(--base-03) bg-(--base-02) px-2.5 py-1 text-xs">
            {body}
        </span>
    );
}

/** Nodes, customers and accounts, at a glance. From the overview, not the record. */
function FleetRow() {
    const { fleet } = useInfra();
    if (!fleet) return null;
    return (
        <section aria-label="Fleet" className="card p-4">
            <div className="grid grid-cols-2 sm:grid-cols-3 xl:grid-cols-6 gap-x-4 gap-y-3">
                {fleetTiles(fleet).map(t => (
                    <div key={t.label} className="flex flex-col gap-0.5 min-w-0">
                        <span className="mono-label truncate">{t.label}</span>
                        <span className="text-lg font-semibold tabular-nums text-(--base-09) leading-tight">{t.value}</span>
                        <span className="text-[11px] text-(--base-06) truncate" title={t.sub}>{t.sub}</span>
                    </div>
                ))}
            </div>
        </section>
    );
}

/**
 * Players on the platform right now. Read live rather than from the record, so
 * it is there even before recording is switched on.
 */
function LivePlayersTile() {
    const { playersOnline, playersServers, playersAt, playersCheckedAt } = useInfra();
    // playersCheckedAt is the render clock: it moves on every finished poll, so
    // the label ages even while polls fail.
    const status = liveStatus(playersAt, Math.max(playersCheckedAt, playersAt ?? 0));
    return (
        <div className="card p-5 flex items-start justify-between gap-4">
            <div className="flex flex-col gap-1 min-w-0 flex-1 max-w-md">
                <span className="mono-label">Players online now</span>
                <span className="font-display text-4xl font-bold tabular-nums text-(--base-09) leading-none">
                    {playersOnline ?? '-'}
                </span>
                <span className="text-[11px] text-(--base-06)">
                    Added up from each running server&apos;s own player count.
                </span>
                {playersServers.length > 0 && (
                    <details className="group mt-2">
                        <summary className="inline-flex items-center gap-1 cursor-pointer select-none list-none rounded-sm text-xs text-(--accent-light) hover:text-(--base-09) focus-visible:outline-2 focus-visible:outline-(--accent) [&::-webkit-details-marker]:hidden">
                            <ChevronDown size={12} className="transition-transform group-open:rotate-180 motion-reduce:transition-none" aria-hidden />
                            {playersServers.length} {playersServers.length === 1 ? 'server' : 'servers'} with players
                        </summary>
                        <ul className="mt-2 max-h-56 overflow-y-auto rounded-md border border-(--base-03) bg-(--base-02) divide-y divide-(--base-03)">
                            {playersServers.map(s => (
                                <li key={s.serverId}>
                                    <Link
                                        href={`/servers/${s.serverId}`}
                                        className="flex items-center justify-between gap-4 px-3 py-1.5 text-sm hover:bg-(--base-03) focus-visible:bg-(--base-03) focus-visible:outline-none"
                                    >
                                        <span className="truncate text-(--base-08)">{s.name}</span>
                                        <span className="font-mono tabular-nums text-(--base-09) shrink-0">{s.players}</span>
                                    </Link>
                                </li>
                            ))}
                        </ul>
                    </details>
                )}
            </div>
            <div className="flex flex-col items-end gap-2 shrink-0">
                <div className="w-9 h-9 rounded-md bg-(--accent-ghost) text-(--accent-light) border border-(--accent-border) flex items-center justify-center">
                    <Users size={18} />
                </div>
                <span className="flex items-center gap-1.5 text-[11px] text-(--base-07)">
                    <span
                        aria-hidden
                        className={`status-dot ${status.live ? 'bg-(--success-light) shadow-[0_0_8px_var(--success-light)]' : 'bg-(--warning-light)'}`}
                    />
                    {status.label}
                </span>
            </div>
        </div>
    );
}

interface ChartProps {
    info: MetricSeriesInfo;
    series: MetricSeries[];
    range: string;
    /** The summary's number for this metric, shown as the chart's headline. */
    headline?: MetricHeadline;
}

function MetricChart({ info, series, range, headline }: ChartProps) {
    // One folded line per metric on this screen. Per-component lines are a
    // different question ("which edge") and belong to a drill-down, not to a
    // page whose job is "what did the platform do".
    const points = series[0]?.points ?? [];
    const data = points.map(p => ({
        t: p.t,
        v: pointValue(p, info.kind),
        min: p.min,
        max: p.max,
    }));

    const peak = data.reduce((m, d) => Math.max(m, d.max), 0);

    return (
        <div className="card p-4 flex flex-col gap-3">
            <div className="flex items-start justify-between gap-3">
                <div className="flex flex-col gap-0.5 min-w-0">
                    <span className="text-sm font-medium text-(--base-09)">{info.label}</span>
                    {info.help && (
                        <span className="text-[11px] text-(--base-06) max-w-md leading-snug">{info.help}</span>
                    )}
                    {headline?.help && (
                        <span className="text-[11px] text-(--base-06) max-w-md leading-snug">{headline.help}</span>
                    )}
                </div>
                {/* The summary's figure where there is one: it is reduced over
                    the raw record, the chart's own over its buckets. */}
                {headline ? (
                    <div className="text-right shrink-0">
                        <div className="text-[11px] text-(--base-06)">{howLabel(headline.how)}</div>
                        <div className="font-display text-2xl font-bold tabular-nums text-(--base-09) leading-tight">
                            {formatMetric(headline.value, headline.unit)}
                        </div>
                    </div>
                ) : (
                    <div className="text-right shrink-0">
                        <div className="text-[11px] text-(--base-06)">
                            {info.kind === 'counter' ? 'Total' : 'Peak'}
                        </div>
                        <div className="text-sm font-mono tabular-nums text-(--base-08)">
                            {formatMetric(
                                info.kind === 'counter'
                                    ? data.reduce((s, d) => s + d.v, 0)
                                    : peak,
                                info.unit,
                            )}
                        </div>
                    </div>
                )}
            </div>

            {data.length === 0 ? (
                <div className="h-40 flex items-center justify-center text-xs text-(--base-06)">
                    Nothing recorded in this period.
                </div>
            ) : (
                <div className="h-40">
                    <ResponsiveContainer width="100%" height="100%">
                        <AreaChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }}>
                            <defs>
                                <linearGradient id={`g-${info.metric}`} x1="0" y1="0" x2="0" y2="1">
                                    <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.35} />
                                    <stop offset="100%" stopColor="var(--accent)" stopOpacity={0.02} />
                                </linearGradient>
                            </defs>
                            <CartesianGrid strokeDasharray="3 3" stroke="var(--base-03)" vertical={false} />
                            <XAxis
                                dataKey="t"
                                tickFormatter={v => formatTick(v, range)}
                                stroke="var(--base-05)"
                                tick={{ fontSize: 10 }}
                                minTickGap={32}
                            />
                            <YAxis
                                stroke="var(--base-05)"
                                tick={{ fontSize: 10 }}
                                width={56}
                                tickFormatter={v => formatMetric(v, info.unit)}
                            />
                            <Tooltip
                                contentStyle={tooltipStyle}
                                labelFormatter={v => fmtDateTime(v as string)}
                                // `unknown`, not `number`, and not by preference:
                                // recharts types the formatter's value as a
                                // union that has been spelled differently
                                // across builds of the same version, and a
                                // narrower parameter is only assignable to the
                                // one that happens to be installed. A parameter
                                // this wide is assignable to all of them.
                                formatter={(v: unknown) => [formatMetric(typeof v === 'number' ? v : 0, info.unit), info.label]}
                            />
                            <Area
                                type="monotone"
                                dataKey="v"
                                stroke="var(--accent)"
                                strokeWidth={1.5}
                                fill={`url(#g-${info.metric})`}
                                isAnimationActive={false}
                            />
                        </AreaChart>
                    </ResponsiveContainer>
                </div>
            )}
        </div>
    );
}

function CoverageLine({ coverage }: { coverage: MetricCoverage | null }) {
    if (!coverage?.since) {
        return (
            <span className="text-xs text-(--base-06)">
                Recording has started; the first samples are still being written.
            </span>
        );
    }
    const since = new Date(coverage.since);
    const days = Math.max(0, Math.floor((Date.now() - since.getTime()) / 86_400_000));
    return (
        <span className="text-xs text-(--base-06)">
            Recording since {fmtDate(since)} ({days} {days === 1 ? 'day' : 'days'}),
            {' '}at {coverage.resolution} resolution.
        </span>
    );
}

export default function StatisticsPanel() {
    const [range, setRange] = useState<string>('30d');
    const [group, setGroup] = useState<string>('Platform');
    const [catalog, setCatalog] = useState<MetricSeriesInfo[]>([]);
    const [coverage, setCoverage] = useState<MetricCoverage | null>(null);
    const [headlines, setHeadlines] = useState<MetricHeadline[]>([]);
    const [charts, setCharts] = useState<Record<string, MetricSeries[]>>({});
    const [loading, setLoading] = useState(true);
    const [chartsLoading, setChartsLoading] = useState(false);
    const [exporting, setExporting] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [unavailable, setUnavailable] = useState<{ reason?: MetricsUnavailableReason; message?: string } | null>(null);

    const groups = useMemo(() => {
        const seen: string[] = [];
        for (const s of catalog) {
            if (!seen.includes(s.group)) seen.push(s.group);
        }
        return seen;
    }, [catalog]);

    const visible = useMemo(
        // Capped, because each one is a query. Twelve charts is already more
        // than anybody reads at once, and the group picker is how you get to
        // the rest.
        () => catalog.filter(s => s.group === group).slice(0, 12),
        [catalog, group],
    );

    const placed = useMemo(
        () => attachHeadlines(visible.map(s => s.metric), headlines),
        [visible, headlines],
    );
    const groupOf = useMemo(() => Object.fromEntries(catalog.map(s => [s.metric, s.group])), [catalog]);

    const loadShell = useCallback(async () => {
        setLoading(true);
        const cat = await getMetricsCatalog();
        if (!cat.available) {
            setUnavailable({ reason: cat.reason, message: cat.message });
            setLoading(false);
            return;
        }
        setUnavailable(null);
        setCatalog(cat.series ?? []);
        setCoverage(cat.coverage ?? null);
        setLoading(false);
    }, []);

    useEffect(() => { void loadShell(); }, [loadShell]);

    // The summary follows the period; the catalog does not, so it is not
    // re-fetched when only the range changes.
    useEffect(() => {
        if (unavailable) return;
        let cancelled = false;
        (async () => {
            const s = await getMetricsSummary(range);
            if (cancelled) return;
            if (!s.available) {
                setUnavailable({ reason: s.reason, message: s.message });
                return;
            }
            setHeadlines(s.headlines ?? []);
            setCoverage(s.coverage ?? null);
        })();
        return () => { cancelled = true; };
    }, [range, unavailable]);

    useEffect(() => {
        if (unavailable || visible.length === 0) return;
        let cancelled = false;
        setChartsLoading(true);
        (async () => {
            // One request for the whole group. A round trip per chart is the
            // difference between one render and twelve.
            const res = await getMetricsSeries({ metrics: visible.map(s => s.metric), range });
            if (cancelled) return;
            setChartsLoading(false);
            if (!res.available) {
                setUnavailable({ reason: res.reason, message: res.message });
                return;
            }
            const by: Record<string, MetricSeries[]> = {};
            for (const s of res.series ?? []) {
                (by[s.metric] ??= []).push(s);
            }
            setCharts(by);
        })();
        return () => { cancelled = true; };
    }, [visible, range, unavailable]);

    const onExport = async (format: 'csv' | 'json') => {
        setExporting(true);
        setError(null);
        try {
            // The WHOLE catalog, not the group on screen: an export is taken to
            // be handed to somebody, and a file holding only the tab that
            // happened to be open is the kind of surprise that is found later.
            await downloadMetricsExport({ metrics: catalog.map(s => s.metric), range }, format);
        } catch (e) {
            setError(e instanceof Error ? e.message : 'Export failed.');
        } finally {
            setExporting(false);
        }
    };

    if (loading) {
        return (
            <div className="flex flex-col gap-4">
                <SkeletonStatGrid tiles={4} />
                <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
                    {Array.from({ length: 4 }).map((_, i) => <SkeletonCard key={i} height="h-56" />)}
                </div>
            </div>
        );
    }

    if (unavailable) {
        return (
            <div className="flex flex-col gap-4">
                <LivePlayersTile />
                <FleetRow />
                <Unavailable reason={unavailable.reason} message={unavailable.message} />
            </div>
        );
    }

    return (
        <div className="flex flex-col gap-4">
            <LivePlayersTile />
            <FleetRow />
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex flex-col gap-1">
                    <CoverageLine coverage={coverage} />
                    {error && <span className="text-xs text-(--error-light)">{error}</span>}
                </div>
                <div className="flex items-center gap-2">
                    <div className="flex items-center gap-0.5 bg-(--base-02) border border-(--base-03) rounded-lg p-1">
                        {RANGES.map(r => (
                            <button
                                key={r.id}
                                onClick={() => setRange(r.id)}
                                aria-pressed={range === r.id}
                                className={`px-3 py-1 rounded-md text-xs font-medium transition-all ${
                                    range === r.id
                                        ? 'bg-(--accent) text-white shadow-sm'
                                        : 'text-(--base-07) hover:text-(--base-09)'
                                }`}
                            >
                                {r.label}
                            </button>
                        ))}
                    </div>
                    <button onClick={() => onExport('csv')} disabled={exporting} className="btn btn-secondary btn-sm">
                        {exporting ? <RefreshCw size={14} className="animate-spin" /> : <Download size={14} />}
                        CSV
                    </button>
                    <button onClick={() => onExport('json')} disabled={exporting} className="btn btn-secondary btn-sm">
                        <Download size={14} />
                        JSON
                    </button>
                </div>
            </div>

            <nav aria-label="Metric groups" className="flex flex-wrap items-center gap-0.5 bg-(--base-02) border border-(--base-03) rounded-lg p-1 w-fit">
                {groups.map(g => (
                    <button
                        key={g}
                        onClick={() => setGroup(g)}
                        aria-pressed={group === g}
                        className={`px-3 py-1.5 rounded-md text-sm font-medium transition-all ${
                            group === g ? 'bg-(--accent) text-white shadow-sm' : 'text-(--base-07) hover:text-(--base-09)'
                        }`}
                    >
                        {g}
                    </button>
                ))}
            </nav>

            {chartsLoading ? (
                <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
                    {visible.map(s => <SkeletonCard key={s.metric} height="h-56" />)}
                </div>
            ) : (
                <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
                    {visible.map(s => (
                        <MetricChart key={s.metric} info={s} series={charts[s.metric] ?? []} range={range} headline={placed.onChart[s.metric]} />
                    ))}
                </div>
            )}

            {placed.rest.length > 0 && (
                <div className="flex flex-col gap-2">
                    <span className="mono-label">Other groups, this period</span>
                    <div className="flex flex-wrap gap-2">
                        {placed.rest.map(h => {
                            const g = groupOf[h.metric];
                            return <HeadlineChip key={h.metric} h={h} onOpen={g ? () => setGroup(g) : undefined} />;
                        })}
                    </div>
                </div>
            )}

            <p className="flex items-start gap-2 text-[11px] text-(--base-06)">
                <Info size={12} className="mt-0.5 shrink-0" />
                <span>
                    Totals are summed across the period and peaks are the highest single reading in it.
                    A gap in a line means nothing was recorded then, which is not the same as a zero.
                </span>
            </p>
        </div>
    );
}
