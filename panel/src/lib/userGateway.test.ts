import { describe, expect, it, vi } from 'vitest';
import {
    gatewayAnswerOf, productLabel, limitText, kitState, routeSuspendedLabel, poolText, truncatedNote, EMPTY_TEXT,
    failureText, hiddenNote, NOT_FOR_ROLE, playersLabel, peakLine, peakSeries, peakDayList, PEAK_NO_DATA,
} from './userGateway';
import { formatDecimalBytes } from './format';

describe('gatewayAnswerOf', () => {
    it('passes a success through as data', () => {
        const res = gatewayAnswerOf(true, 200, { success: true, userId: 'u1', kits: [] });
        expect(res.success).toBe(true);
        if (res.success) expect(res.data.userId).toBe('u1');
    });
    it.each([
        [{ error: 'User not found' }, 404, 'User not found'],
        [{ message: 'Forbidden here' }, 403, 'Forbidden here'],
        // The gateway gate answers {error, code}.
        [{ error: 'This platform uses the managed gateway.', code: 'gateway_remote' }, 409, 'This platform uses the managed gateway.'],
        [{}, 500, 'Request failed (500)'],
    ])('%j (%s) -> the server message', (body, status, want) => {
        expect(gatewayAnswerOf(false, status, body)).toEqual({ success: false, status, message: want });
    });
});

describe('labels', () => {
    it.each([['managed', 'Managed gateway'], ['route_only', 'Route-only'], ['none', 'No gateway product'], ['other', 'other']])(
        'product %s -> %s', (p, want) => expect(productLabel(p)).toBe(want));
    it.each([[null, 'No limit'], [undefined, 'No limit'], [0, 'None'], [3, '3']])(
        'limit %s -> %s', (l, want) => expect(limitText(l)).toBe(want));
});

describe('kitState', () => {
    it.each([
        [{ online: true }, 'Online'],
        [{ online: false }, 'Offline'],
        // Core could not ask: never shown as offline.
        [{ online: null }, 'Unknown'],
        [{ online: true, retireAt: '2026-11-14T08:00:00Z' }, 'Being replaced'],
    ])('%j -> %s', (kit, want) => expect(kitState(kit).label).toBe(want));
});

describe('routeSuspendedLabel', () => {
    it.each([
        [{}, null],
        [{ suspendedAt: '2026-11-11T12:00:00Z' }, 'Suspended'],
        [{ suspendedAt: '2026-11-11T12:00:00Z', suspendedReason: 'abuse report' }, 'Suspended: abuse report'],
    ])('%j -> %s', (r, want) => expect(routeSuspendedLabel(r)).toBe(want));
});

describe('poolText', () => {
    it('is null without a pool', () => expect(poolText(undefined, formatDecimalBytes)).toBeNull());
    it('reads in decimal units with the purchase note', () => {
        expect(poolText({ includedGb: 5000, maxPurchaseGb: null, usedBytes: 1_250_000_000_000, percent: 25 }, formatDecimalBytes))
            .toBe('25% of the pool used: 1.3 TB of 5 TB included. More can be bought.');
    });
    it('caps the percent at 100 and says nothing more can be bought', () => {
        expect(poolText({ includedGb: 0, maxPurchaseGb: 0, usedBytes: 10, percent: 100 }, formatDecimalBytes))
            .toBe('100% of the pool used: 10 B of 0 B included. No extra traffic can be bought.');
        expect(poolText({ includedGb: 1, maxPurchaseGb: 0, usedBytes: 5e9, percent: 500 }, formatDecimalBytes)).toMatch(/^100% /);
    });
});

describe('truncatedNote and empty states', () => {
    it('only a cut list gets a notice', () => {
        expect(truncatedNote(false, 200)).toBeNull();
        expect(truncatedNote(true, 200)).toBe('Showing the first 200. This account has more.');
    });
    it('every section has an empty line', () => {
        for (const v of Object.values(EMPTY_TEXT)) expect(v.length).toBeGreaterThan(0);
    });
});

describe('failureText', () => {
    it('a 403 reads as one neutral line, not Forbidden', () => {
        expect(failureText({ status: 403, message: 'Forbidden' })).toBe(NOT_FOR_ROLE);
    });
    it.each([[404, 'User not found'], [500, "Failed to load the user's gateway"], [undefined, 'Connection failed']])(
        '%s keeps the server message', (status, message) => expect(failureText({ status, message })).toBe(message));
});

describe('playersLabel', () => {
    it.each([
        [undefined, 'None'],
        [{ java: 0, bedrock: 0 }, 'None'],
        [{ java: 3, bedrock: 0 }, '3 Java'],
        [{ java: 0, bedrock: 2 }, '2 Bedrock'],
        [{ java: 3, bedrock: 1 }, '3 Java, 1 Bedrock'],
    ])('%j -> %s', (p, want) => expect(playersLabel(p)).toBe(want));
});

describe('peakLine', () => {
    it.each([
        ['Peak today', undefined, true, 'Peak today: none recorded'],
        ['Peak today', { java: 0, bedrock: 0, total: 0 }, true, 'Peak today: none recorded'],
        ['Peak today', { java: 3, bedrock: 1, total: 4 }, true, 'Peak today: at least 4 (3 Java, 1 Bedrock)'],
        ['Peak today', { java: 5, bedrock: 0, total: 5 }, true, 'Peak today: at least 5 (5 Java)'],
        ['Peak in the last 30 days', { java: 9, bedrock: 2, total: 10 }, false, 'Peak in the last 30 days: at least 10'],
    ] as const)('%s %j editions=%s -> %s', (label, w, editions, want) => {
        expect(peakLine(label, w, editions)).toBe(want);
    });
});

describe('peakSeries', () => {
    const row = (day: string, total: number) => ({ day, java: total, bedrock: 0, total });
    // A day without a row was never measured (peaks start on deploy day):
    // null, so the chart draws no zero it did not see (E2E D8).
    it('is 30 days without data without rows', () => {
        const s = peakSeries([], '2026-11-14');
        expect(s).toHaveLength(30);
        expect(s.every(v => v === null)).toBe(true);
    });
    it('runs oldest to newest with gaps as no data', () => {
        const s = peakSeries([row('2026-11-14', 4), row('2026-11-12', 7), row('2026-10-16', 2)], '2026-11-14');
        expect(s).toHaveLength(30);
        expect(s[29]).toBe(4); // today, last
        expect(s[28]).toBeNull(); // 2026-11-13, no row
        expect(s[27]).toBe(7);
        expect(s[0]).toBe(2); // 29 days ago, first
    });
    it('follows the server day, not a browser clock a day off', () => {
        vi.useFakeTimers();
        try {
            vi.setSystemTime(new Date('2026-11-15T23:30:00Z')); // browser a day ahead
            const days = [row('2026-11-14', 4), row('2026-11-13', 6)];
            const s = peakSeries(days, '2026-11-14');
            expect(s.slice(-2)).toEqual([6, 4]); // the server's today is the last bar
            vi.setSystemTime(new Date('2026-11-13T00:30:00Z')); // a day behind
            expect(peakSeries(days, '2026-11-14')).toEqual(s);
        } finally {
            vi.useRealTimers();
        }
    });
    it('crosses a month and leaves out days outside the window', () => {
        const s = peakSeries([row('2026-12-01', 3), row('2026-11-30', 5), row('2026-11-01', 99)], '2026-12-01');
        expect(s.slice(-2)).toEqual([5, 3]);
        expect(s).not.toContain(99);
    });
});

describe('peakDayList', () => {
    const row = (day: string, total: number) => ({ day, java: total, bedrock: 0, total });
    it('lists every day from today back to the oldest row, a gap as no data', () => {
        const l = peakDayList([row('2026-11-14', 4), row('2026-11-11', 7)], '2026-11-14');
        expect(l.map(d => d.day)).toEqual(['2026-11-14', '2026-11-13', '2026-11-12', '2026-11-11']);
        expect(l.map(d => d.peak?.total ?? PEAK_NO_DATA)).toEqual([4, PEAK_NO_DATA, PEAK_NO_DATA, 7]);
    });
    it('is today as no data when only older rows exist', () => {
        expect(peakDayList([row('2026-11-13', 2)], '2026-11-14').map(d => d.peak?.total ?? null)).toEqual([null, 2]);
    });
    it('is empty without rows', () => {
        expect(peakDayList([], '2026-11-14')).toEqual([]);
    });
});

describe('hiddenNote', () => {
    it.each([
        [undefined, 'billing', null],
        [[], 'usage', null],
        [['apiKeys'], 'billing', null],
        [['billing', 'usage', 'apiKeys'], 'billing', 'Plan, billing and limits are not shown for your role.'],
        [['billing', 'usage'], 'usage', 'Traffic is not shown for your role.'],
        [['apiKeys'], 'apiKeys', 'API keys are not shown for your role.'],
    ] as const)('%j %s -> %s', (hidden, block, want) => {
        expect(hiddenNote(hidden as never, block)).toBe(want);
    });
});
