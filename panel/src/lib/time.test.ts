import { describe, it, expect, afterEach } from 'vitest';
import { consoleClock, fmtDateTime, fmtTime, prefOptions, setTimePrefs, uses12h } from './time';

afterEach(() => setTimePrefs({ timezone: '', timeFormat: 'auto' }));

describe('time prefs', () => {
    it('shows a timestamp in the account zone, not the browser one', () => {
        setTimePrefs({ timezone: 'Europe/Berlin', timeFormat: '24h' });
        // 12:00 UTC in July is 14:00 in Berlin.
        expect(fmtTime('2026-07-15T12:00:00Z', { hour: '2-digit', minute: '2-digit' })).toMatch(/14:00/);
        setTimePrefs({ timezone: 'America/New_York', timeFormat: '24h' });
        expect(fmtTime('2026-07-15T12:00:00Z', { hour: '2-digit', minute: '2-digit' })).toMatch(/08:00/);
    });

    it('follows the clock format', () => {
        setTimePrefs({ timezone: 'UTC', timeFormat: '12h' });
        expect(fmtDateTime('2026-07-15T15:00:00Z')).toMatch(/PM/i);
        setTimePrefs({ timezone: 'UTC', timeFormat: '24h' });
        expect(fmtDateTime('2026-07-15T15:00:00Z')).toMatch(/15:00/);
        expect(uses12h()).toBe(false);
    });

    it('lets the browser decide what was never chosen', () => {
        expect(prefOptions({ timezone: '', timeFormat: 'auto' })).toEqual({});
    });

    it('returns an unparseable value as it came', () => {
        expect(fmtDateTime('not a date')).toBe('not a date');
    });
});

describe('consoleClock', () => {
    it('rewrites the leading clock to 12 hours', () => {
        expect(consoleClock('[14:02:11 INFO]: Done', true)).toBe('[2:02:11 PM INFO]: Done');
        expect(consoleClock('[00:15:00] [Server thread/INFO]: x', true)).toBe('[12:15:00 AM] [Server thread/INFO]: x');
        expect(consoleClock('[12:00:00 WARN]: y', true)).toBe('[12:00:00 PM WARN]: y');
        expect(consoleClock('\x1b[33m[09:01:02 INFO]: z', true)).toBe('\x1b[33m[9:01:02 AM INFO]: z');
    });
    it('leaves 24 hours and lines without a clock alone', () => {
        expect(consoleClock('[14:02:11 INFO]: Done', false)).toBe('[14:02:11 INFO]: Done');
        expect(consoleClock('at 14:02:11 something', true)).toBe('at 14:02:11 something');
    });
});
