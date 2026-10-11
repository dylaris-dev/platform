import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { formatMetric, pointValue, seriesQueryString, type MetricPoint } from '@/lib/api/metrics';

const point = (p: Partial<MetricPoint>): MetricPoint => ({
    t: '2026-09-01T00:00:00Z', min: 0, max: 0, sum: 0, count: 0, avg: 0, ...p,
});

describe('reading a bucket', () => {
    it('a counter is its total and a gauge is its average', () => {
        // The one thing this screen can get wrong without looking wrong. A
        // bucket holding four samples that summed to 200 is either "200 things
        // happened" or "it sat at 50" - and which one is decided by the kind,
        // not by the numbers.
        const p = point({ sum: 200, count: 4, avg: 50, min: 40, max: 60 });
        expect(pointValue(p, 'counter')).toBe(200);
        expect(pointValue(p, 'gauge')).toBe(50);
    });

    it('an empty bucket reads as zero rather than NaN', () => {
        // count 0 would make sum/count a NaN, and recharts draws a NaN as a gap
        // that looks exactly like missing data.
        const p = point({});
        expect(Number.isNaN(pointValue(p, 'gauge'))).toBe(false);
        expect(pointValue(p, 'counter')).toBe(0);
    });
});

describe('formatting by unit', () => {
    it('scales bytes by 1024 and bits per second by 1000', () => {
        // Not interchangeable: storage is binary and network throughput is
        // decimal, and using one scale for both misstates a headline figure by
        // about seven percent - enough to be wrong and not enough to be noticed.
        expect(formatMetric(1024, 'bytes')).toBe('1 KB');
        expect(formatMetric(1000, 'bps')).toBe('1 kbit/s');
        expect(formatMetric(1024, 'bps')).toBe('1.02 kbit/s');
    });

    it('spells throughput in bits, not in the ambiguous Mbps', () => {
        // The unit is the whole finding here. "Mbps" is read as megaBYTES often
        // enough that a saturated gigabit link looked like 12% of one, so this
        // screen spells it the way the Bandwidth tab does. The value is
        // unchanged - only the label was ever wrong.
        expect(formatMetric(2_500_000, 'bps')).toBe('2.5 Mbit/s');
        expect(formatMetric(2_500_000_000, 'bps')).toBe('2.5 Gbit/s');
        expect(formatMetric(0, 'bps')).toBe('0 bit/s');
    });

    it('renders percentages, durations and counts in their own terms', () => {
        expect(formatMetric(99.95, 'percent')).toBe('100%');
        expect(formatMetric(66.666, 'percent')).toBe('66.7%');
        expect(formatMetric(90, 'seconds')).toBe('2m');
        expect(formatMetric(172800, 'seconds')).toBe('2d');
        expect(formatMetric(12, 'count')).toBe('12');
        expect(formatMetric(3.42, 'count')).toBe('3.4');
        expect(formatMetric(2_500_000, 'count')).toBe('2.5M');
    });

    it('a whole count keeps no decimal', () => {
        // "12.0 nodes" reads as a bug; "3.4 players on average" does not.
        expect(formatMetric(12, 'count')).toBe('12');
    });
});

describe('the series request', () => {
    it('asks for several metrics in one round trip', () => {
        const qs = seriesQueryString({ metrics: ['a.b', 'c.d'], range: '30d' });
        expect(qs).toContain('metric=a.b%2Cc.d');
        expect(qs).toContain('range=30d');
    });

    it('omits the optional filters rather than sending empty ones', () => {
        // An empty subject sent as `subject=` would filter for the empty string,
        // which is the folded row - so every per-component chart would come back
        // holding nothing.
        const qs = seriesQueryString({ metrics: ['a.b'], range: '24h' });
        expect(qs).not.toContain('subject=');
        expect(qs).not.toContain('split=');
    });
});

describe('the statistics tab', () => {
    const APP = join(__dirname, '../../app/(authed)/infrastructure');

    it('does not gate itself in the client', () => {
        // Deliberate, and the opposite of every other gated tab. Whether there
        // is anything recorded depends on a feature flag and on whether the
        // metrics database opened - both SERVER facts. A client-side copy of
        // that answer is a second source of truth that can disagree with the
        // endpoint, and the way it disagrees is a blank screen with no reason on
        // it. The page renders the reason the server gives instead.
        const src = readFileSync(join(APP, 'statistics/page.tsx'), 'utf8');
        // The RENDERED element, not the word: the file explains in a comment
        // why it has no guard, and matching the prose would fail on the
        // explanation itself.
        expect(src).not.toContain('<TabGuard');
        expect(src).not.toContain('useInfra(');
    });

    it('distinguishes "switched off" from "cannot reach the database"', () => {
        // One is the default and one is a fault. Collapsing them sends an
        // operator looking for a broken database when nobody turned the feature
        // on - and the fix for each is in a different place.
        const panel = readFileSync(join(__dirname, 'StatisticsPanel.tsx'), 'utf8');
        expect(panel).toContain("'disabled'");
        const api = readFileSync(join(__dirname, '../../lib/api/metrics.ts'), 'utf8');
        expect(api).toContain("'unavailable'");
        expect(api).toContain("'disabled'");
    });

    it('exports the whole catalog, not the group on screen', () => {
        // An export is taken to be handed to somebody. A file holding only the
        // tab that happened to be open is the kind of surprise found after it
        // has been sent.
        const panel = readFileSync(join(__dirname, 'StatisticsPanel.tsx'), 'utf8');
        const call = panel.slice(panel.indexOf('downloadMetricsExport('));
        expect(call.slice(0, 120)).toContain('catalog.map');
    });
});
