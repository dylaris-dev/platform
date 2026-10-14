import { describe, it, expect } from 'vitest';
import { latestValue, chartSeries, appendSamples, LIVE_SAMPLES } from './statsSeries';

describe('latestValue', () => {
    it('reads the last sample when it has the value', () => {
        expect(latestValue([{ heap: 100 }, { heap: 250 }], 'heap')).toBe(250);
    });

    // The bug this exists for: the heap field is omitted on any tick with no
    // fresh GC reading, so a running server showed 0 MB whenever the final
    // sample fell between collections.
    it('falls back to the last sample that has one', () => {
        expect(latestValue([{ heap: 250 }, {}, {}], 'heap')).toBe(250);
    });

    it('is 0 when no sample ever carried the value', () => {
        const empty: { heap?: number }[] = [{}, {}];
        expect(latestValue(empty, 'heap')).toBe(0);
        expect(latestValue([], 'heap')).toBe(0);
    });

    // 0 is a legitimate reading and must not be skipped over as if missing.
    it('accepts a genuine zero', () => {
        expect(latestValue([{ heap: 250 }, { heap: 0 }], 'heap')).toBe(0);
    });

    it('skips values that are not usable numbers', () => {
        expect(latestValue([{ heap: 250 }, { heap: NaN }], 'heap')).toBe(250);
        expect(latestValue([{ heap: 250 }, { heap: undefined }], 'heap')).toBe(250);
    });
});

describe('chartSeries', () => {
    // The History chart drew a straight line across the hours a server was off.
    it('breaks the line where samples stop for longer than the gap', () => {
        const out = chartSeries([{ ts: 0, cpu: 1 }, { ts: 120, cpu: 2 }, { ts: 4000, cpu: 3 }], 300);
        expect(out).toEqual([{ ts: 0, cpu: 1 }, { ts: 120, cpu: 2 }, { ts: 121 }, { ts: 4000, cpu: 3 }]);
    });

    it('adds nothing to a continuous series and sorts by time', () => {
        expect(chartSeries([{ ts: 240 }, { ts: 0 }, { ts: 120 }], 300).map(p => p.ts)).toEqual([0, 120, 240]);
    });

    it('carries the sparse heap forward, but never across a gap', () => {
        const out = chartSeries([
            { ts: 0, javaHeapUsed: 500 },
            { ts: 2 },
            { ts: 4, javaHeapUsed: null },
            { ts: 100 },
        ], 10);
        expect(out).toEqual([
            { ts: 0, javaHeapUsed: 500 },
            { ts: 2, javaHeapUsed: 500 },
            { ts: 4, javaHeapUsed: 500 },
            { ts: 5 },
            { ts: 100 },
        ]);
    });
});

describe('appendSamples', () => {
    const s = (ts: number) => ({ ts });

    it('keeps the newest LIVE_SAMPLES in order', () => {
        const all = Array.from({ length: 100 }, (_, i) => s(i + 1));
        const out = appendSamples([], all);
        expect(out).toHaveLength(LIVE_SAMPLES);
        expect(out[0].ts).toBe(41);
        expect(out[out.length - 1].ts).toBe(100);
    });

    it('starts again when the clock jumps back past what a replay covers', () => {
        const prev = [s(10_000), s(10_002)];
        expect(appendSamples(prev, [s(4_000), s(4_002)]).map(p => p.ts)).toEqual([4_000, 4_002]);
    });

    it('drops a replayed sample the chart already holds', () => {
        const prev = [s(1), s(2), s(3)];
        expect(appendSamples(prev, [s(2), s(3)])).toBe(prev);
        expect(appendSamples(prev, [s(3), s(4)]).map(p => p.ts)).toEqual([1, 2, 3, 4]);
    });
});
