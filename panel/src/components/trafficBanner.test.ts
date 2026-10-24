import { describe, it, expect } from 'vitest';
import { dataBannerState, trafficBannerState, TRAFFIC_WARN_PCT } from './trafficBanner';
import type { MyTrafficStatus } from '@/lib/api/billing';

const t = (usedGb: number, ceilingGb: number, billingEnabled = false): MyTrafficStatus => ({
    usedGb,
    ceilingGb,
    // Mirrors what Core computes, integer-truncated - which is exactly the value
    // this must NOT use to decide whether the tenant is over.
    pct: ceilingGb > 0 ? Math.floor((usedGb * 100) / ceilingGb) : 100,
    billingEnabled,
});

describe('trafficBannerState', () => {
    it('says nothing while the tenant is well inside their allowance', () => {
        expect(trafficBannerState(t(500, 1100), 'active')).toBe('none');
    });

    it('warns once past the threshold', () => {
        const warnGb = Math.ceil((1100 * TRAFFIC_WARN_PCT) / 100);
        expect(trafficBannerState(t(warnGb - 20, 1100), 'active')).toBe('none');
        expect(trafficBannerState(t(warnGb, 1100), 'active')).toBe('approaching');
    });

    // The bug this file exists for. Core reports 1100 of 1100 GB as pct 100, but
    // the store cuts at strictly MORE than the ceiling, so nothing is stopped -
    // and a banner reading the percentage told the tenant their servers were down
    // while they were serving players.
    it('does not claim a stop at exactly the ceiling, where usage is still free', () => {
        expect(t(1100, 1100).pct).toBe(100); // the misleading input, pinned
        expect(trafficBannerState(t(1100, 1100), 'active')).toBe('approaching');
        expect(trafficBannerState(t(1100, 1100), 'suspended')).not.toBe('stopped');
    });

    // pct truncates the other way too: 1101 of 1100 is 100.09%, which floors to
    // 100 and is indistinguishable from the case above. Only the raw comparison
    // separates them.
    it('separates one GB over from exactly at, which pct cannot', () => {
        expect(t(1101, 1100).pct).toBe(t(1100, 1100).pct);
        expect(trafficBannerState(t(1101, 1100), 'active')).toBe('over');
        expect(trafficBannerState(t(1101, 1100), 'suspended')).toBe('stopped');
    });

    // The guard runs hourly, so "past the ceiling" and "stopped" are different
    // moments. Saying "stopped" during that window is a false alarm; saying
    // "you are fine" is a missed one.
    it('distinguishes over-but-running from actually stopped', () => {
        expect(trafficBannerState(t(5000, 1100), 'active')).toBe('over');
        expect(trafficBannerState(t(5000, 1100), 'past_due')).toBe('over');
        expect(trafficBannerState(t(5000, 1100), 'suspended')).toBe('stopped');
    });

    it('stays silent for a tenant who agreed to be billed, however far over', () => {
        expect(trafficBannerState(t(50_000, 1100, true), 'active')).toBe('none');
        expect(trafficBannerState(t(50_000, 1100, true), 'suspended')).toBe('none');
    });

    it('stays silent when there is nothing to report', () => {
        expect(trafficBannerState(null, 'active')).toBe('none');
        expect(trafficBannerState(undefined, 'active')).toBe('none');
        // No ceiling means the store told us nothing, not that the tenant is over
        // an allowance of zero. A self-hosted install lands here.
        expect(trafficBannerState(t(500, 0), 'active')).toBe('none');
    });
});

describe('dataBannerState', () => {
    const base = { usedGb: 0, ceilingGb: 100, pct: 0, billingEnabled: false };
    const relay = (pct: number) => ({ kind: 'relay', region: '*', usedGb: pct, includedGb: 100, pct, warn: 0 });
    it('says nothing below 90 % or with data overage on', () => {
        expect(dataBannerState({ ...base, pools: [relay(89)] })).toBe('none');
        expect(dataBannerState({ ...base, dataBillingEnabled: true, dataThrottled: true, pools: [relay(150)] })).toBe('none');
    });
    it('warns from 90 % and says throttled once Core throttles', () => {
        expect(dataBannerState({ ...base, pools: [relay(90)] })).toBe('warn');
        expect(dataBannerState({ ...base, dataThrottled: true, pools: [relay(100)] })).toBe('throttled');
    });
    it('ignores player pools', () => {
        expect(dataBannerState({ ...base, pools: [{ ...relay(99), kind: 'edge', region: 'eu' }] })).toBe('none');
    });
});
