import { describe, it, expect } from 'vitest';
import { currentSkip, relativeTime } from './backupJobStatus';

describe('currentSkip', () => {
    it('warns when the skip is newer than the last run', () => {
        expect(currentSkip({
            lastRunAt: '2026-10-10T10:00:00Z',
            lastSkipAt: '2026-10-11T10:00:00Z',
            lastSkipReason: 'server is migrating',
        })).toEqual({ at: '2026-10-11T10:00:00Z', reason: 'server is migrating' });
    });

    it('warns when the job never ran but skipped', () => {
        expect(currentSkip({ lastSkipAt: '2026-10-11T10:00:00Z' })?.reason).toBe('no reason recorded');
    });

    it('is quiet once a run happened after the skip', () => {
        expect(currentSkip({
            lastRunAt: '2026-10-11T12:00:00Z',
            lastSkipAt: '2026-10-11T10:00:00Z',
            lastSkipReason: 'x',
        })).toBeNull();
    });

    it('is quiet without a skip', () => {
        expect(currentSkip({ lastRunAt: '2026-10-11T12:00:00Z' })).toBeNull();
        expect(currentSkip({ lastSkipAt: 'garbage' })).toBeNull();
    });
});

describe('relativeTime', () => {
    const now = Date.parse('2026-10-11T12:00:00Z');
    it.each([
        ['2026-10-11T11:55:00Z', '5m ago'],
        ['2026-10-11T15:00:00Z', 'in 3h'],
        ['2026-10-13T12:00:00Z', 'in 2d'],
        ['2026-10-11T12:00:20Z', 'in under a minute'],
        ['2026-10-11T11:59:50Z', 'just now'],
    ])('%s -> %s', (iso, want) => {
        expect(relativeTime(iso, now)).toBe(want);
    });

    it('dashes a missing or bad time', () => {
        expect(relativeTime(null, now)).toBe('—');
        expect(relativeTime('nope', now)).toBe('—');
    });
});
