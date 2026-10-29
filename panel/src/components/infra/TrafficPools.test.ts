import { describe, it, expect } from 'vitest';
import { boxesFor, isCapped, poolRegion, poolTone, productShares, formatBytes, voiceShare, bedrockShare } from './TrafficPools';
import type { TrafficPool } from '@/lib/api/billing';

const pool = (p: Partial<TrafficPool>): TrafficPool => ({
    kind: 'edge', region: 'eu-central', usedGb: 0, includedGb: 1000, pct: 0, warn: 0, ...p,
});

describe('boxesFor', () => {
    it('splits the pools into player traffic and data traffic', () => {
        // Two boxes because the platform meters two different things and prices
        // neither together: what players move at the edge, and what beam
        // carries. One list mixed them into a single column of bars where the
        // kind was a prefix on each label.
        const got = boxesFor([
            pool({ kind: 'edge', region: 'eu-central' }),
            pool({ kind: 'relay', region: '*' }),
            pool({ kind: 'edge', region: 'us-east' }),
        ]);
        expect(got.map(b => b.kind)).toEqual(['edge', 'relay']);
        expect(got[0].pools.map(p => p.region)).toEqual(['eu-central', 'us-east']);
        expect(got[1].pools).toHaveLength(1);
    });

    it('drops a box with no pools rather than heading an empty one', () => {
        expect(boxesFor([pool({ kind: 'edge' })]).map(b => b.kind)).toEqual(['edge']);
    });

    it('survives an absent pool list', () => {
        expect(boxesFor(undefined)).toEqual([]);
    });
});

describe('isCapped', () => {
    // A pool with no configured allowance used to be dropped entirely, and that
    // is how "where is my data traffic" happens: nobody had set a relay
    // allowance, so real transferred bytes were rendered nowhere at all. The
    // original reasoning was only half right - there is nothing to draw a BAR
    // against, which is not a reason to hide the NUMBER.
    it('reports an unlimited pool as uncapped rather than hiding it', () => {
        expect(isCapped(pool({ includedGb: null, usedGb: 9000 }))).toBe(false);
    });

    it('treats a zero allowance as uncapped, which would draw a permanently full bar', () => {
        expect(isCapped(pool({ includedGb: 0 }))).toBe(false);
    });

    it('reports a real allowance as capped', () => {
        expect(isCapped(pool({ includedGb: 1000 }))).toBe(true);
    });
});

describe('poolRegion', () => {
    it('names the wildcard region instead of printing an asterisk', () => {
        expect(poolRegion(pool({ region: '*' }))).toBe('All regions');
    });

    it('leaves a real region alone', () => {
        expect(poolRegion(pool({ region: 'eu-central' }))).toBe('eu-central');
    });
});

describe('productShares', () => {
    it('names the two products as the infrastructure tabs name them', () => {
        const got = productShares(pool({ byProductBytes: { byon: 5_000_000_000, route: 9_000_000_000 } }));
        // Largest first: the split exists to answer "what is eating this", and
        // the answer should be the first thing read.
        expect(got.map(s => s.label)).toEqual(['Protected addresses', 'Bring your own node']);
        expect(got[0].bytes).toBe(9_000_000_000);
    });

    it('shows an unattributed share as unattributed rather than folding it into a product', () => {
        // Rows written before the split existed carry an empty product. Those
        // bytes are real and are in the billing total; picking one of the two
        // products for them would be a wrong answer where this is an honest
        // missing one.
        const got = productShares(pool({ byProductBytes: { '': 3_000_000_000 } }));
        expect(got.map(s => s.label)).toEqual(['Not attributed']);
    });

    it('drops empty shares and survives a pool with no split at all', () => {
        expect(productShares(pool({ byProductBytes: { byon: 0 } }))).toEqual([]);
        expect(productShares(pool({}))).toEqual([]);
    });
});

describe('formatBytes', () => {
    it('does not round a real share down to nothing', () => {
        // The whole reason the wire carries the split in bytes: two shares of
        // 400 MB beside a 1 GB total, both rendered "0 GB", reads as a bug.
        expect(formatBytes(400_000_000)).toBe('400 MB');
        expect(formatBytes(1_500_000_000)).toBe('1.5 GB');
    });

    it('drops the decimal once the number is big enough not to need it', () => {
        expect(formatBytes(120_000_000_000)).toBe('120 GB');
    });

    it('has an answer for zero and for nonsense', () => {
        expect(formatBytes(0)).toBe('0 MB');
        expect(formatBytes(NaN)).toBe('0 MB');
    });
});

describe('poolTone', () => {
    it('says nothing below the first threshold', () => {
        expect(poolTone(0)).toBeNull();
    });

    it('warns at 80 and escalates at 90', () => {
        // Two thresholds, not one: the first is a heads-up with most of the
        // month's remainder left to act on, the second is the last one that
        // still arrives before the allowance is gone.
        expect(poolTone(80)?.label).toBe('80% used');
        expect(poolTone(90)?.label).toBe('90% used');
    });

    it('reads past the allowance as over, not as 90%', () => {
        expect(poolTone(100)?.label).toBe('over the allowance');
    });
});

describe('bedrockShare', () => {
    it('is absent when Core sent no Bedrock bytes or none moved', () => {
        expect(bedrockShare(pool({ usedGb: 1 }))).toBeNull();
        expect(bedrockShare(pool({ usedGb: 1, bedrockBytes: 0 }))).toBeNull();
    });

    it('is absent on the data-traffic pool', () => {
        expect(bedrockShare(pool({ kind: 'relay', usedGb: 1, bedrockBytes: 300_000_000 }))).toBeNull();
    });

    it('is its own subset, independent of voice, floored and capped like voiceShare', () => {
        const p = pool({ usedGb: 1, voiceBytes: 500_000_000, bedrockBytes: 299_999_999 });
        expect(bedrockShare(p)).toEqual({ bytes: 299_999_999, pct: 29 });
        expect(voiceShare(p)).toEqual({ bytes: 500_000_000, pct: 50 });
        expect(bedrockShare(pool({ usedGb: 1, bedrockBytes: 1_400_000_000 }))?.pct).toBe(100);
    });
});

describe('voiceShare', () => {
    it('is absent when Core sent no voice bytes', () => {
        expect(voiceShare(pool({ usedGb: 1 }))).toBeNull();
    });

    it('is absent for a pool that saw no voice', () => {
        expect(voiceShare(pool({ usedGb: 1, voiceBytes: 0 }))).toBeNull();
    });

    it('is absent on the data-traffic pool, which carries no voice', () => {
        expect(voiceShare(pool({ kind: 'relay', usedGb: 1, voiceBytes: 300_000_000 }))).toBeNull();
    });

    it('is the share of the pool in decimal GB, rounded down like pct', () => {
        expect(voiceShare(pool({ usedGb: 1, voiceBytes: 300_000_000 }))).toEqual({ bytes: 300_000_000, pct: 30 });
        expect(voiceShare(pool({ usedGb: 1, voiceBytes: 299_999_999 }))).toEqual({ bytes: 299_999_999, pct: 29 });
    });

    it('is 0% of a pool truncated to 0 GB, still with its bytes', () => {
        expect(voiceShare(pool({ usedGb: 0, voiceBytes: 400_000_000 }))).toEqual({ bytes: 400_000_000, pct: 0 });
    });

    it('never claims more than the whole pool', () => {
        expect(voiceShare(pool({ usedGb: 1, voiceBytes: 1_400_000_000 }))?.pct).toBe(100);
    });
});
