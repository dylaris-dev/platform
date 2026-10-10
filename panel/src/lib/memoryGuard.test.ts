import { describe, expect, it } from 'vitest';
import { memoryGuardPatch, showOomBanner } from './memoryGuard';

describe('showOomBanner', () => {
    const at = '2026-10-10T12:00:00Z';
    const t = Date.parse(at);
    const oom = { lastCrashReason: 'oom_killed', lastCrashAt: at };

    it('shows a recent OOM kill', () => {
        expect(showOomBanner(oom, t + 60_000, null)).toBe(true);
        expect(showOomBanner(oom, t + 23 * 3600_000, null)).toBe(true);
    });
    it('hides after 24 hours', () => {
        expect(showOomBanner(oom, t + 24 * 3600_000, null)).toBe(false);
    });
    it('hides the crash the viewer dismissed, but not a newer one', () => {
        expect(showOomBanner(oom, t + 60_000, at)).toBe(false);
        expect(showOomBanner({ ...oom, lastCrashAt: '2026-10-10T13:00:00Z' }, t + 3600_000, at)).toBe(true);
    });
    it('hides other reasons, no crash, and an older Core', () => {
        expect(showOomBanner({ lastCrashReason: 'other', lastCrashAt: at }, t, null)).toBe(false);
        expect(showOomBanner({ lastCrashReason: null, lastCrashAt: null }, t, null)).toBe(false);
        expect(showOomBanner({}, t, null)).toBe(false);
    });
});

describe('memoryGuardPatch', () => {
    it('sends only a change', () => {
        expect(memoryGuardPatch('stop', 'stop')).toBeUndefined();
        expect(memoryGuardPatch('stop', 'off')).toEqual({ memoryGuardAction: 'off' });
        expect(memoryGuardPatch('restart', 'stop')).toEqual({ memoryGuardAction: 'stop' });
    });
    it('sends nothing to an older Core that has no such field', () => {
        expect(memoryGuardPatch(undefined, 'off')).toBeUndefined();
    });
});
