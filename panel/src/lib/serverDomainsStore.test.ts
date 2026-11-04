import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('@/lib/api/serverDomains', () => ({ getServerDomains: vi.fn() }));
import { getServerDomains } from '@/lib/api/serverDomains';
import {
    applyResult, trustedDomains, subscribeServerDomains, getServerDomainsState, maxStateAgeMs, MAX_AGE_MS, POLL_MS, type DomainsState,
} from './serverDomainsStore';

const ok = (fqdn = 'a.x.io', state = 'green', observedAt = new Date(1000).toISOString(), intervalSeconds?: number) => ({
    success: true, domains: [{ fqdn, state, observedAt }], zones: [], directAddress: '1.2.3.4:25565',
    limits: { used: 1, max: null, serverUsed: 1, serverMax: null }, intervalSeconds,
}) as never;
const empty: DomainsState = { data: null, fetchedAt: 0, stale: false, hidden: false, error: '' };
const mock = vi.mocked(getServerDomains);

describe('applyResult / trustedDomains', () => {
    it('success replaces the snapshot', () => {
        const s = applyResult(empty, ok(), 1000);
        expect(s).toMatchObject({ fetchedAt: 1000, stale: false, hidden: false });
        expect(trustedDomains(s, 1000)).toHaveLength(1);
    });
    it('a 404 clears the snapshot', () => {
        const s = applyResult(applyResult(empty, ok(), 1000), { success: false, status: 404 } as never, 2000);
        expect(s.data).toBeNull();
        expect(s.hidden).toBe(true);
        expect(trustedDomains(s, 2000)).toEqual([]);
    });
    it('a failed poll keeps the list but marks it stale', () => {
        const s = applyResult(applyResult(empty, ok(), 1000), { success: false, status: 500, message: 'boom' } as never, 2000);
        expect(s.data).not.toBeNull();
        expect(s).toMatchObject({ stale: true, error: 'boom', fetchedAt: 1000 });
    });
    it('a green domain is trusted only from a snapshot younger than 45 s', () => {
        const s = applyResult(empty, ok(), 1000);
        expect(trustedDomains(s, 1000 + MAX_AGE_MS)).toHaveLength(1);
        expect(trustedDomains(s, 1000 + MAX_AGE_MS + 1)).toEqual([]);
    });
    it('a fresh poll still distrusts a state Core observed too long ago', () => {
        // Interval 30 s: the floor of 90 s applies.
        const now = 1_000_000;
        const at = (ago: number) => new Date(now - ago).toISOString();
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', at(90_000), 30), now), now)).toHaveLength(1);
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', at(90_001), 30), now), now)).toEqual([]);
        // Interval 120 s: two intervals.
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', at(240_000), 120), now), now)).toHaveLength(1);
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', at(240_001), 120), now), now)).toEqual([]);
        // An older Core without the interval: 90 s.
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', at(90_001)), now), now)).toEqual([]);
        // Never observed.
        expect(trustedDomains(applyResult(empty, ok('a.x.io', 'green', ''), now), now)).toEqual([]);
        expect(maxStateAgeMs(undefined)).toBe(90_000);
    });
});

describe('poller ref-counting', () => {
    let visibility = 'visible';
    let dispatch: () => void;
    beforeEach(() => {
        vi.useFakeTimers();
        mock.mockReset();
        mock.mockResolvedValue(ok());
        visibility = 'visible';
        let ls: (() => void)[] = [];
        dispatch = () => ls.forEach(f => f());
        vi.stubGlobal('document', {
            get visibilityState() { return visibility; },
            addEventListener: (_t: string, f: () => void) => { ls.push(f); },
            removeEventListener: (_t: string, f: () => void) => { ls = ls.filter(x => x !== f); },
        });
    });
    afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

    it('shares one poller between subscribers and stops with the last one', async () => {
        const u1 = subscribeServerDomains('7', () => {});
        const u2 = subscribeServerDomains('7', () => {});
        expect(mock).toHaveBeenCalledTimes(1);
        await vi.advanceTimersByTimeAsync(POLL_MS);
        expect(mock).toHaveBeenCalledTimes(2);
        u1();
        await vi.advanceTimersByTimeAsync(POLL_MS);
        expect(mock).toHaveBeenCalledTimes(3);
        u2();
        await vi.advanceTimersByTimeAsync(POLL_MS * 3);
        expect(mock).toHaveBeenCalledTimes(3);
        expect(getServerDomainsState('7').data).toBeNull();
    });

    it('stops while hidden and restarts with an immediate fetch when visible', async () => {
        const u = subscribeServerDomains('8', () => {});
        await vi.advanceTimersByTimeAsync(0);
        visibility = 'hidden';
        dispatch();
        await vi.advanceTimersByTimeAsync(POLL_MS * 4);
        expect(mock).toHaveBeenCalledTimes(1);
        visibility = 'visible';
        dispatch();
        expect(mock).toHaveBeenCalledTimes(2);
        u();
    });
});
