"use client";

import { useEffect, useMemo, useState } from 'react';
import { Cpu, MemoryStick, ArrowUpCircle } from 'lucide-react';
import { getGatewayBandwidthOverview } from '@/lib/api';
import { getMetricsSeries, type MetricsSeriesResponse } from '@/lib/api/metrics';
import Sparkline from '@/components/infra/Sparkline';
import { SpliceVersionSummary } from './InfraCards';
import { useInfra } from './context';
import { spliceImageMismatch } from '@/lib/spliceDrift';
import { formatBitsPerSec, type GatewayBandwidthOverview, type GatewayComponentView } from '@/lib/bandwidth';
import {
    LOAD_RANGES, loadSeriesRequest, indexLoadSeries, type LoadRange, type LoadHistory,
} from '@/lib/gatewayLoad';
import type { GatewayEdge } from '@/lib/api';

/**
 * What runs the gateway: edges, warp leaders and beam relays, one row each,
 * with what the machine costs and how that has moved.
 *
 * The three sections rather than one table, and this is the whole reason: each
 * kind has a number the others do not have. An edge carries PLAYERS and pins a
 * splice version, a warp leader holds PEERS, a beam relay has TRANSFERS in
 * flight. A single table would show all three columns for all three kinds and
 * leave two thirds of every row structurally blank.
 *
 * Three sources, each because it is the only one that has the answer:
 *   - the infrastructure overview, for edges - it knows the address, the splice
 *     version, and whether the running splice is the pinned image;
 *   - the bandwidth mirror, for the LIVE CPU and RAM of warp and beam;
 *   - the long-term metrics database, for the HISTORY behind the graphs. The
 *     mirror holds one instant and nothing else, so it cannot draw a line.
 */

export default function GatewayPanel() {
    const { edges } = useInfra();
    const [overview, setOverview] = useState<GatewayBandwidthOverview | null>(null);
    const [range, setRange] = useState<LoadRange>('1h');
    const [history, setHistory] = useState<MetricsSeriesResponse | null>(null);

    useEffect(() => {
        const load = () => { getGatewayBandwidthOverview().then(setOverview).catch(() => { /* keep the last snapshot */ }); };
        load();
        const t = setInterval(load, 10000);
        return () => clearInterval(t);
    }, []);

    // Every line in one request: six metrics, split per component. The API caps
    // a request at 24 metrics, so this stays one round trip however many
    // machines are in the fleet - the split happens server-side.
    useEffect(() => {
        let cancelled = false;
        const fetchHistory = () => {
            getMetricsSeries(loadSeriesRequest(range))
                .then(res => { if (!cancelled) setHistory(res); })
                .catch(() => { if (!cancelled) setHistory(null); });
        };
        fetchHistory();
        // The buckets are a minute wide, so anything faster re-sends the same points.
        const t = setInterval(fetchHistory, 60000);
        return () => { cancelled = true; clearInterval(t); };
    }, [range]);

    const load: LoadHistory = useMemo(() => indexLoadSeries(history?.series), [history]);

    const of = (kind: string) => (overview?.components ?? []).filter(c => c.component === kind);
    const warp = of('warp');
    const beam = of('beam');

    // Why there is no line, when there is none. Recording is OFF by default and
    // that is not a fault, so it is said once at the top rather than as six
    // identical empty charts that look like a broken screen.
    const noHistory = history && history.available === false
        ? history.reason === 'disabled'
            ? 'Long-term statistics are switched off, so the graphs have nothing to draw. Turn them on under Settings, Features.'
            : history.message || 'The statistics database could not be reached, so the graphs are empty.'
        : null;

    return (
        <div className="flex flex-col gap-5">
            <SpliceVersionSummary edges={edges} />

            <div className="flex items-center justify-between gap-3 flex-wrap">
                <span className="mono-label">Load history</span>
                <div className="flex gap-1" role="group" aria-label="Time range">
                    {LOAD_RANGES.map(r => (
                        <button
                            key={r}
                            onClick={() => setRange(r)}
                            aria-pressed={range === r}
                            className={`px-2.5 py-1 rounded-sm text-xs font-medium transition-colors ${
                                range === r ? 'bg-(--base-04) text-(--base-09)' : 'text-(--base-07) hover:text-(--base-09)'
                            }`}
                        >
                            {r}
                        </button>
                    ))}
                </div>
            </div>

            {noHistory && (
                <div className="rounded-lg border border-(--base-03) bg-(--base-02) px-4 py-2.5 text-xs text-(--base-06)">
                    {noHistory}
                </div>
            )}

            <Section
                title="Edges"
                count={edges.length}
                // Named rather than implied: an operator staring at an empty
                // section should not have to guess whether it is broken or idle.
                empty="No edges registered. Edges auto-discover through Redis once one is deployed."
            >
                {edges.map(e => <EdgeRow key={e.edge_id} edge={e} load={load} />)}
            </Section>

            <Section
                title="Warp leaders"
                count={warp.length}
                empty="No warp leader reporting. A leader appears here once it publishes telemetry."
            >
                {warp.map(c => (
                    <ComponentRow
                        key={c.id}
                        comp={c}
                        load={load}
                        // Both numbers, because the gap between them IS the
                        // health of the overlay: a configured peer is a row,
                        // an active one is a tunnel that handshook recently.
                        extra={`${fmtGauge(c, 'peers_active')} / ${fmtGauge(c, 'peers')} peers up`}
                    />
                ))}
            </Section>

            <Section
                title="Beam relays"
                count={beam.length}
                empty="No beam relay reporting. A relay appears here once it publishes telemetry."
            >
                {beam.map(c => (
                    <ComponentRow key={c.id} comp={c} load={load} extra={`${fmtGauge(c, 'active_transfers')} transfers`} />
                ))}
            </Section>
        </div>
    );
}

// fmtGauge reads one of a component's own gauges. A gauge a component never
// published is shown as "-" and never as 0: "nothing connected" and "this build
// does not report it" are opposite claims, and a 0 states the alarming one.
function fmtGauge(c: GatewayComponentView, name: string): string {
    const v = c.gauges?.[name];
    return v === undefined ? '-' : String(Math.round(v));
}

function Section({ title, count, empty, children }: {
    title: string; count: number; empty: string; children: React.ReactNode;
}) {
    return (
        <section className="flex flex-col gap-2">
            <div className="flex items-center gap-2">
                <h3 className="text-sm font-semibold text-(--base-09)">{title}</h3>
                <span className="text-[10px] font-mono tabular-nums px-1.5 py-0.5 rounded-full bg-(--base-03) text-(--base-06)">{count}</span>
            </div>
            {count === 0 ? (
                <div className="card p-6 text-center text-(--base-06) text-sm">{empty}</div>
            ) : (
                <div className="card divide-y divide-(--base-03)">{children}</div>
            )}
        </section>
    );
}

/**
 * One measurement: its own box, its own chart, its live value beside it.
 *
 * The scale is fixed to 0-100 and a line marks the halfway point. Both matter:
 * scaled to the window's own maximum, a machine idling between 2 and 4 percent
 * draws the same alarming sawtooth as one swinging between 40 and 90, and
 * without a reference line a small box cannot show WHERE a flat line sits.
 *
 * CPU and RAM get a box each rather than two lines in one. They are unrelated
 * judgements - a busy CPU is work being done, a full RAM is a machine about to
 * be in trouble - and sharing an axis made the reader match colours to decide
 * which of the two they were looking at.
 */
function LoadBox({ label, icon, pct, values, color }: {
    label: string;
    icon: React.ReactNode;
    pct: number;
    values: number[];
    color: string;
}) {
    return (
        <div className="rounded-md border border-(--base-03) bg-(--base-02) px-3 py-2 flex flex-col gap-1.5 w-64">
            <div className="flex items-center gap-1.5 mono-label text-(--base-06)">
                <span className="text-(--base-05)">{icon}</span>
                {label}
                <span className="ml-auto text-(--base-05)">0-100%</span>
            </div>
            <div className="flex items-center gap-3">
                <div className="flex-1 min-w-0">
                    <Sparkline
                        series={[{ values, color, fill: true }]}
                        max={100}
                        height={56}
                        grid={[0.5]}
                        title={`${label} history`}
                        empty="no history yet"
                    />
                </div>
                <span className="text-lg font-mono tabular-nums text-(--base-09) w-14 text-right shrink-0">
                    {pct.toFixed(0)}%
                </span>
            </div>
        </div>
    );
}

/** Both boxes for one component. */
function LoadBlock({ cpu, ram, history }: {
    cpu: number; ram: number; history?: { cpu: number[]; ram: number[] };
}) {
    return (
        <div className="flex items-center gap-3 flex-wrap">
            <LoadBox label="CPU" icon={<Cpu size={11} />} pct={cpu} values={history?.cpu ?? []} color="var(--accent)" />
            <LoadBox label="RAM" icon={<MemoryStick size={11} />} pct={ram} values={history?.ram ?? []} color="var(--primary)" />
        </div>
    );
}

function StatusDot({ online }: { online: boolean }) {
    return (
        <span
            className={`w-2 h-2 rounded-full shrink-0 ${online ? 'bg-(--success-light) shadow-[0_0_6px_var(--success-light)]' : 'bg-(--error)'}`}
            aria-label={online ? 'online' : 'offline'}
        />
    );
}

/** One instance: identity on the left, what it costs on the right. */
function Row({ head, meta, load }: { head: React.ReactNode; meta: React.ReactNode; load: React.ReactNode }) {
    return (
        <div className="flex items-center justify-between gap-6 px-4 py-6 flex-wrap">
            <div className="flex flex-col gap-1 min-w-56">
                <div className="flex items-center gap-2 flex-wrap text-sm">{head}</div>
                <div className="flex items-center gap-3 flex-wrap text-xs text-(--base-06) font-mono">{meta}</div>
            </div>
            {load}
        </div>
    );
}

function EdgeRow({ edge, load }: { edge: GatewayEdge; load: LoadHistory }) {
    const online = edge.status === 'online';
    const s = edge.stats;
    const running = edge.splice_version || '';
    const latest = edge.splice_version_latest || '';
    // Behind: the pin names an older version than the edge image carries.
    const behind = !!latest && running !== latest;
    // A DIFFERENT fault, invisible to the version comparison: the running
    // splice is not the image the pin resolves to, at the same version string.
    const wrongImage = spliceImageMismatch(edge.splice_image_running || '', edge.splice_image_available || '');

    return (
        <Row
            head={
                <>
                    <StatusDot online={online} />
                    <span className="font-medium text-(--base-09) truncate">{edge.name}</span>
                    {behind ? (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-(--warning)/10 mono-label text-(--warning-light)">
                            <ArrowUpCircle size={9} /> splice {running ? `v${running}` : 'unknown'} &rarr; v{latest}
                        </span>
                    ) : wrongImage ? (
                        <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-(--error)/10 mono-label text-(--error-light)">
                            <ArrowUpCircle size={9} /> splice v{running || latest} &middot; wrong image
                        </span>
                    ) : (running || latest) ? (
                        <span className="mono-label text-(--base-06)">splice v{running || latest}</span>
                    ) : null}
                </>
            }
            meta={
                <>
                    <span>{edge.ip}:{edge.service_port}</span>
                    {edge.region && <span>{edge.region}</span>}
                    {online && s ? (
                        <>
                            <span className="text-(--base-07)">{s.active_mc_streams} players</span>
                            <Throughput txBps={s.tx_speed * 8} rxBps={s.rx_speed * 8} />
                        </>
                    ) : (
                        <span className="text-(--error-light)">offline</span>
                    )}
                </>
            }
            load={
                online && s
                    ? <LoadBlock cpu={s.cpu} ram={s.ram_pct} history={load.get(`edge:${edge.edge_id}`)} />
                    : null
            }
        />
    );
}

/**
 * Both directions, because both have their own ceiling.
 *
 * This line used to show transmit alone, which is the direction that saturates
 * first on an edge and so reads as "the" throughput - but the link carries its
 * rated speed each way at the same time, and an inbound-saturated host looked
 * idle here. Named out/in rather than tx/rx: up and down are ambiguous about
 * whose point of view they are, and tx/rx means nothing to most readers.
 */
function Throughput({ txBps, rxBps }: { txBps: number; rxBps: number }) {
    return (
        <span title="out = what this machine sends (tx), in = what it receives (rx). A full-duplex link carries its rated speed in each direction at once.">
            out {formatBitsPerSec(txBps)} &middot; in {formatBitsPerSec(rxBps)}
        </span>
    );
}

function ComponentRow({ comp, extra, load }: { comp: GatewayComponentView; extra: string; load: LoadHistory }) {
    return (
        <Row
            head={
                <>
                    {/* Alive is always true here: a component has a mirror entry
                        only while it is reporting, and the entry expires on its own. */}
                    <StatusDot online={comp.alive} />
                    <span className="font-medium text-(--base-09) truncate">{comp.id}</span>
                </>
            }
            meta={
                <>
                    <span>{comp.host}</span>
                    {comp.region && <span>{comp.region}</span>}
                    <span className="text-(--base-07)">{extra}</span>
                    <Throughput txBps={comp.txBps} rxBps={comp.rxBps} />
                    {comp.uptimeSec !== undefined && comp.uptimeSec > 0 && (
                        <span title="process uptime">up {formatUptime(comp.uptimeSec)}</span>
                    )}
                </>
            }
            load={
                <LoadBlock cpu={comp.cpuPct} ram={comp.ramPct} history={load.get(`${comp.component}:${comp.id}`)} />
            }
        />
    );
}

// formatUptime renders a process age at one unit of precision. Days matter,
// seconds do not: what a reader takes from this is "did this restart recently",
// not the exact age.
export function formatUptime(sec: number): string {
    if (sec < 60) return `${Math.floor(sec)}s`;
    if (sec < 3600) return `${Math.floor(sec / 60)}m`;
    if (sec < 86400) return `${Math.floor(sec / 3600)}h`;
    return `${Math.floor(sec / 86400)}d`;
}
