// Helpers for reading a headline number off a stats series.

/**
 * The most recent sample that actually carries `key`.
 *
 * Stats samples are sparse by design: the JVM heap field is omitted on any tick
 * where no fresh post-GC reading was available, so reading only the final
 * sample reported a running server as using 0 MB whenever the last tick
 * happened to fall between garbage collections.
 *
 * Returns 0 when no sample in the window carries the key, which is the honest
 * answer for a series that has never had one.
 */
export function latestValue<T extends object>(series: T[], key: keyof T): number {
    for (let i = series.length - 1; i >= 0; i--) {
        const v = series[i][key];
        if (typeof v === 'number' && Number.isFinite(v)) return v;
    }
    return 0;
}

/** A point that only breaks the line: it carries a time and no values. */
export interface GapPoint {
    ts: number;
}

/**
 * Stats points ready for a time-scaled chart: sorted by `ts` (seconds), with a
 * value-less point wherever two neighbours are more than `maxGapSec` apart, so
 * a server that was off draws as a gap rather than a straight line across the
 * time it was off. The chart must keep connectNulls off for that to hold.
 *
 * The heap reading is sparse (only ticks with a fresh post-GC value carry it),
 * so it is carried forward within a run: without that it would draw as dots.
 * It never crosses a gap.
 */
export function chartSeries<T extends { ts: number; javaHeapUsed?: number | null }>(
    points: T[],
    maxGapSec: number,
): Array<T | GapPoint> {
    const sorted = [...points].sort((a, b) => a.ts - b.ts);
    const out: Array<T | GapPoint> = [];
    let heap: number | null = null;
    for (let i = 0; i < sorted.length; i++) {
        const p = sorted[i];
        if (i > 0 && p.ts - sorted[i - 1].ts > maxGapSec) {
            out.push({ ts: sorted[i - 1].ts + 1 });
            heap = null;
        }
        if (typeof p.javaHeapUsed === 'number' && Number.isFinite(p.javaHeapUsed)) {
            heap = p.javaHeapUsed;
            out.push(p);
        } else {
            out.push(heap === null ? p : { ...p, javaHeapUsed: heap });
        }
    }
    return out;
}

/** Samples the live chart keeps: two minutes at the node's 2 s interval. */
export const LIVE_SAMPLES = 60;

// How far back a replayed buffer reaches: LIVE_SAMPLES at 2 s, with slack.
const REPLAY_SPAN_SEC = LIVE_SAMPLES * 2 + 10;

/**
 * Appends a batch of live samples, keeping the newest LIVE_SAMPLES. A sample
 * not newer than the last one kept, but within what a replay covers, is
 * dropped: a reopened stream replays the buffer, and those samples are
 * already on the chart. One further back starts the series again - the
 * node's clock is behind the one before (a server moved to another node), and
 * dropping everything older than the last old sample froze the chart.
 */
export function appendSamples<T extends { ts: number }>(prev: T[], batch: T[]): T[] {
    let kept = prev;
    let last = prev.length > 0 ? prev[prev.length - 1].ts : -Infinity;
    const fresh: T[] = [];
    for (const s of batch) {
        if (s.ts < last - REPLAY_SPAN_SEC) {
            kept = [];
            fresh.length = 0;
        } else if (s.ts <= last) {
            continue;
        }
        fresh.push(s);
        last = s.ts;
    }
    if (kept === prev && fresh.length === 0) return prev;
    return [...kept, ...fresh].slice(-LIVE_SAMPLES);
}
