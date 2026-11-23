"use client";

import { ArrowUp, ArrowDown } from 'lucide-react';
import Tooltip from '@/components/ui/Tooltip';
import Sparkline from '@/components/infra/Sparkline';
import {
    formatBitsPerSec, utilTone, barWidthPct,
    type GatewayComponentView, type GatewayHostView, type UtilTone,
} from '@/lib/bandwidth';

/**
 * One host, as a row: what the machine carries on the left, what each service
 * on it carries on the right.
 *
 * This replaced a four-column grid (a host rail plus one column per kind). The
 * grid lined co-located services up correctly and was still hard to read: three
 * columns of mostly-empty cells, because most hosts do not run all three, and
 * the eye had to travel across a whole screen width to answer a question about
 * ONE machine.
 *
 * The cap and the totals stay on the left and appear once, because a cap is a
 * property of the HOST - co-located services describe one shared uplink, so
 * showing it per service would be the same number repeated with a different
 * meaning implied.
 */

export interface RowKind {
    key: string;
    label: string;
}

/**
 * How tall a service chart is, in pixels.
 *
 * Named once because three things have to agree on it: the chart, the box that
 * draws the middle line across it, and the placeholder for a service that is
 * not on this host. If they drift apart, rows stop lining up - which is the one
 * thing this layout exists for.
 */
export const CHART_H = 150;

/** Outbound and inbound, each with ONE colour used for its line and its number. */
export const DIR_COLOR = { tx: 'var(--accent)', rx: 'var(--primary)' } as const;


const toneBar: Record<UtilTone, string> = {
    ok: 'bg-(--success-light)',
    warn: 'bg-(--warning-light)',
    crit: 'bg-(--error)',
};

export default function BandwidthHostRow({
    host, kinds, cells, seriesFor, scaleBps, range, active, onSelectHost, onSelectComponent,
}: {
    host: GatewayHostView;
    kinds: readonly RowKind[];
    cells: Record<string, GatewayComponentView[]>;
    /** Both directions of one component's history, as plain numbers. */
    seriesFor: (c: GatewayComponentView) => { tx: number[]; rx: number[] };
    /**
     * The fallback scale, used only where no cap is configured: the busiest
     * series on the screen. Where a cap IS known the chart is scaled to it, so
     * a given height means the same share of the link in every box.
     */
    scaleBps: number;
    /** How far back the charts reach, shown in each one. */
    range: string;
    active?: boolean;
    onSelectHost?: () => void;
    onSelectComponent?: (c: GatewayComponentView) => void;
}) {
    return (
        <div className={`srv-surface flex items-stretch gap-5 p-5 flex-wrap ${active ? 'ring-1 ring-(--accent)' : ''}`}>
            {/* A FIXED width, not a minimum. With min-w the rail was as wide as
                its content, so a host with no cap (and therefore no bars) got a
                narrower rail and every chart in that row started further left.
                Charts that do not line up cannot be compared down a column,
                which is the one thing this layout exists for. */}
            <button
                onClick={onSelectHost}
                className="flex flex-col gap-2.5 text-left w-64 shrink-0 self-center rounded-md focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                aria-pressed={active}
            >
                <div className="flex items-center gap-2 flex-wrap">
                    <span className={`font-medium ${host.host ? 'text-(--base-09)' : 'text-(--warning-light)'}`}>
                        {host.host || 'no host reported'}
                    </span>
                    {host.capMismatch && (
                        <span className="srv-chip bg-(--warning-ghost)! text-(--warning-light)!">
                            cap mismatch
                        </span>
                    )}
                </div>
                {/* ONE cap, TWO bars. Ethernet is full duplex: a 1 Gbit port
                    sends a gigabit and receives a gigabit at the same time, so
                    the two directions do not compete for one budget. Measuring
                    them against a single shared bar would have been wrong in
                    the direction that hides a problem - a saturated downlink
                    reading as comfortable because the uplink was quiet. */}
                <Tooltip
                    content={host.capKnown
                        ? `BANDWIDTH_MBIT is ${host.budgetMbit} Mbit/s. Ethernet is full duplex, so that is the ceiling for EACH direction, not for the two together.`
                        : 'No BANDWIDTH_MBIT set on this host, so there is nothing to measure against.'}
                >
                    <div className="srv-num text-xs text-(--base-06)">
                        {host.capKnown ? `${(host.budgetMbit / 1000).toFixed(1)} Gbit/s each way` : 'no cap set'}
                    </div>
                </Tooltip>
                <DirBlock dir="tx" bps={host.txBps} pct={host.utilPct} known={host.capKnown} />
                <DirBlock dir="rx" bps={host.rxBps} pct={host.utilPctRx} known={host.capKnown} />
            </button>

            <div className="flex items-stretch gap-3 flex-wrap flex-1">
                {/* min-w-72 is a real minimum, not a preference: below it the
                    chart is too narrow for its own cap label and the rates
                    beside it wrap into two lines of digits. The row wraps
                    instead, stacking the services under the host - taller, but
                    still readable, which is the right way round. */}
                {kinds.map(k => (
                    <div key={k.key} className="flex flex-col gap-2 flex-1 min-w-72">
                        {(cells[k.key] ?? []).length === 0 ? (
                            <div className="flex-1 rounded-lg border border-dashed border-(--base-04) flex flex-col items-center justify-center gap-0.5" style={{ minHeight: CHART_H + 38 }}>
                                <span className="srv-label">{k.label}</span>
                                <span className="text-xs text-(--base-06)">not on this host</span>
                            </div>
                        ) : (
                            (cells[k.key] ?? []).map(c => (
                                <ServiceBox
                                    key={`${c.component}:${c.id}`}
                                    label={k.label}
                                    comp={c}
                                    values={seriesFor(c)}
                                    scaleBps={scaleBps}
                                    range={range}
                                    onSelect={() => onSelectComponent?.(c)}
                                />
                            ))
                        )}
                    </div>
                ))}
            </div>
        </div>
    );
}

/** One service on this host: its outbound/inbound history and its live rates. */
function ServiceBox({ label, comp, values, scaleBps, range, onSelect }: {
    label: string;
    comp: GatewayComponentView;
    values: { tx: number[]; rx: number[] };
    scaleBps: number;
    range: string;
    onSelect?: () => void;
}) {
    return (
        <button
            onClick={onSelect}
            className="rounded-lg bg-(--base-03) px-3 py-2 flex flex-col gap-1.5 text-left flex-1 transition-colors hover:bg-(--base-04) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
        >
            <div className="srv-label flex items-center gap-2">
                {label}
                {/* The instance id, and it is not decoration: a host can run two
                    edges, and then this is the only thing telling the two boxes
                    apart. It is the same string the logs, the Gateway tab and
                    the Redis keys use, which is what makes it worth the width. */}
                <Tooltip content={`${label} id "${comp.id}": the same name in the logs and on the Gateway tab`}>
                    <span className="srv-num text-(--base-07) truncate">{comp.id}</span>
                </Tooltip>
            </div>
            {/* ONE line across the whole box at the middle, chart and numbers
                alike. It is what makes the mirrored chart readable without a
                legend: everything above the line LEAVES this host, everything
                below it ARRIVES. The two numbers are the same size and sit one
                either side of that same line, so where they sit says which
                direction they belong to before the colour does. */}
            <div className="relative flex items-stretch gap-3" style={{ height: CHART_H }}>
                <div className="absolute inset-x-0 top-1/2 h-px bg-(--base-04)" aria-hidden />
                {/* The FRAME is the cap. Its top edge is 100% of the configured
                    limit outbound and its bottom edge is 100% inbound, so the
                    height of a line is the share of the link it is using and
                    means the same thing in every box on the screen.

                    The alternative - scaling to the busiest value on screen -
                    is what this replaced, and it moved: the same 480 Mbit drew
                    tall on a quiet afternoon and short on a busy one, so no
                    chart could be compared with another or with itself an hour
                    earlier.

                    Without a cap there is no 100% to draw, so the frame goes
                    dashed and the scale falls back to the busiest series on the
                    screen. A solid frame there would claim a limit nobody set. */}
                <div className="flex-1 min-w-0 relative">
                    <DirChart
                        values={values}
                        scaleBps={comp.capKnown ? comp.capMbit * 1_000_000 : scaleBps}
                        label={`${comp.component} ${comp.id}`}
                    />
                    {/* Drawn OVER the chart rather than around it. A border in
                        the layout would add its two pixels to the height, and
                        the chart's own middle would then sit one pixel off the
                        line this box draws across itself - which is the one
                        thing the line exists to make exact. */}
                    <div
                        className={`absolute inset-0 rounded border pointer-events-none ${
                            comp.capKnown ? 'border-(--base-05)' : 'border-dashed border-(--base-04)'
                        }`}
                        aria-hidden
                    />
                    <span className="absolute right-1.5 top-1 srv-num text-[10px] text-(--base-06) leading-none pointer-events-none">
                        {comp.capKnown ? `${formatBitsPerSec(comp.capMbit * 1_000_000)} cap` : 'no cap'}
                    </span>
                    {/* In the chart, not only in the control above it. The
                        window is what every height on the screen is relative
                        to, and a reader who scrolled past the switcher has no
                        way back to it except to scroll up and trust memory. */}
                    <span className="absolute left-1.5 bottom-1 srv-num text-[10px] text-(--base-06) leading-none pointer-events-none">
                        last {range}
                    </span>
                </div>
                <div className="flex flex-col shrink-0 relative w-28">
                    <div className="flex-1 flex items-end justify-end pb-1.5">
                        <Rate dir="tx" bps={comp.txBps} className="text-sm" tinted />
                    </div>
                    <div className="flex-1 flex items-start justify-end pt-1.5">
                        <Rate dir="rx" bps={comp.rxBps} className="text-sm" tinted />
                    </div>
                </div>
            </div>
        </button>
    );
}

/**
 * A throughput reading, with the direction spelled out.
 *
 * OUT and IN rather than up and down. Up and down are the words a reader knows
 * from their own connection, and there they mean the OPPOSITE of what they mean
 * here: an edge's "upload" is what its players download. Out and in are stated
 * relative to this host and cannot be read the wrong way round.
 *
 * "tx" and "rx" stay alongside, because they are what the API, the logs and
 * every gateway component call these - dropping them would cut the connection
 * to everything a reader might correlate this with.
 */
function Rate({ dir, bps, className = '', tinted }: {
    dir: 'tx' | 'rx'; bps: number; className?: string; tinted?: boolean;
}) {
    const up = dir === 'tx';
    return (
        <Tooltip
            content={up
                ? 'tx / out: what this host sends OUT. For an edge that is what its players download.'
                : 'rx / in: what this host takes IN. For an edge that is what its players upload.'}
        >
            <span
                className={`inline-flex items-center gap-1 srv-num ${className}`}
                style={tinted ? { color: DIR_COLOR[dir] } : undefined}
            >
                {up ? <ArrowUp size={14} className="shrink-0" aria-hidden="true" /> : <ArrowDown size={14} className="shrink-0" aria-hidden="true" />}
                <span className="text-(--base-06)">{up ? 'tx/out' : 'rx/in'}</span>
                {formatBitsPerSec(bps)}
            </span>
        </Tooltip>
    );
}

/**
 * One direction of the host total: the rate on its own line, the bar under it.
 *
 * Two lines rather than one, because on one line the rate, the bar and the
 * percentage competed for a rail narrow enough to keep every chart in the row
 * aligned - and the bar lost, ending up too short to read a level off.
 *
 * Both directions get a bar against the SAME cap, because a full-duplex link
 * carries its rated speed each way at once. Until this screen did that, every
 * utilisation figure on the platform was outbound only, and a host saturating
 * its inbound direction showed nothing and alerted nobody.
 */
function DirBlock({ dir, bps, pct, known }: { dir: 'tx' | 'rx'; bps: number; pct: number; known: boolean }) {
    return (
        <div className="flex flex-col gap-1 w-full">
            <Rate dir={dir} bps={bps} className="text-xs text-(--base-08)" />
            {known ? (
                <div className="flex items-center gap-2">
                    <span className="flex-1 h-1.5 rounded-full bg-(--base-03) overflow-hidden">
                        <span
                            className={`block h-full rounded-full transition-all duration-500 ${toneBar[utilTone(pct)]}`}
                            style={{ width: `${barWidthPct(pct)}%` }}
                        />
                    </span>
                    <span className="srv-num text-xs text-(--base-07) w-9 text-right">
                        {pct.toFixed(0)}%
                    </span>
                </div>
            ) : (
                <span className="text-xs text-(--base-06)">no cap to measure against</span>
            )}
        </div>
    );
}

/**
 * The two directions in one chart, mirrored around the middle.
 *
 * Zero in the centre, outbound growing up, inbound growing down - the
 * arrangement every network graph uses, and the reason is that it needs no
 * legend: the picture itself says which is which. The alternatives (both drawn
 * from the same floor, or two separate strips) were compared side by side and
 * lost - one relies on colour alone, the other halves the height each direction
 * gets.
 *
 * The baseline is drawn by the BOX, across the numbers as well, so the chart
 * does not draw a second one at the same y.
 */
function DirChart({ values, scaleBps, label }: {
    values: { tx: number[]; rx: number[] };
    scaleBps: number;
    label: string;
}) {
    return (
        <Sparkline
            series={[
                { values: values.tx, color: DIR_COLOR.tx, fill: true, direction: 'up' },
                { values: values.rx, color: DIR_COLOR.rx, fill: true, direction: 'down' },
            ]}
            max={scaleBps} height={CHART_H} showBaseline={false}
            title={`${label}: outbound above the line, inbound below it`} empty="no history yet"
        />
    );
}
