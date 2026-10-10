"use client";

import { useCallback, useEffect, useMemo, useState } from 'react';
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid } from 'recharts';
import { BarChart3, Download, Info, RefreshCw } from 'lucide-react';
import { SkeletonCard, SkeletonStatGrid } from '@/components/Skeleton';
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
    if (range === '24h') return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    if (range === '365d') return d.toLocaleDateString([], { month: 'short', year: '2-digit' });
    return d.toLocaleDateString([], { day: '2-digit', month: 'short' });
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

function HeadlineCard({ h }: { h: MetricHeadline }) {
    const how = h.how === 'peak' ? 'Peak' : h.how === 'total' ? 'Total' : 'Average';
    return (
        <div className="card p-4 flex flex-col gap-1">
            <span className="text-[11px] uppercase tracking-wide text-(--base-06)">{h.label}</span>
            <span className="text-2xl font-semibold tabular-nums text-(--base-09)">
                {formatMetric(h.value, h.unit)}
            </span>
            <span className="text-[11px] text-(--base-06)">{how} over the period</span>
        </div>
    );
}

interface ChartProps {
    info: MetricSeriesInfo;
    series: MetricSeries[];
    range: string;
}

function MetricChart({ info, series, range }: ChartProps) {
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
                <div className="flex flex-col gap-0.5">
                    <span className="text-sm font-medium text-(--base-09)">{info.label}</span>
                    {info.help && (
                        <span className="text-[11px] text-(--base-06) max-w-md leading-snug">{info.help}</span>
                    )}
                </div>
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
                                labelFormatter={v => new Date(v as string).toLocaleString()}
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
            Recording since {since.toLocaleDateString()} ({days} {days === 1 ? 'day' : 'days'}),
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
        return <Unavailable reason={unavailable.reason} message={unavailable.message} />;
    }

    return (
        <div className="flex flex-col gap-4">
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

            {headlines.length > 0 && (
                <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-4 gap-3">
                    {headlines.map(h => <HeadlineCard key={h.metric} h={h} />)}
                </div>
            )}

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
                        <MetricChart key={s.metric} info={s} series={charts[s.metric] ?? []} range={range} />
                    ))}
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
