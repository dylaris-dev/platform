import { describe, expect, it } from 'vitest';
import { throttleChanged, withRateFloor } from './nodeBackupThrottleHelpers';

describe('withRateFloor', () => {
    it.each([
        [null, null],
        [0, 100], // the LimitField switch turned off: never a 0 B/s cap
        [1, 1],
        [250, 250],
    ])('%s -> %s', (input, want) => {
        expect(withRateFloor(input, 100)).toBe(want);
    });
});

describe('throttleChanged', () => {
    const base = { uplinkMbit: 1000, readMBps: null, spoolEnabled: false, spoolMaxGB: 0 };
    it('is false for the same values', () => {
        expect(throttleChanged(base, { ...base })).toBe(false);
    });
    it.each([
        { uplinkMbit: null },
        { readMBps: 40 },
        { spoolEnabled: true },
        { spoolMaxGB: 50 },
    ])('sees %o', change => {
        expect(throttleChanged(base, { ...base, ...change })).toBe(true);
    });
});
