import { describe, expect, it } from 'vitest';
import {
    gatewayAnswerOf, productLabel, limitText, kitState, routeSuspendedLabel, poolText, truncatedNote, EMPTY_TEXT,
    failureText, hiddenNote, NOT_FOR_ROLE,
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
