import { describe, expect, it } from 'vitest';
import { anchoredTimes, buildSchedule, describeSchedule, editorZone, fmtClock, parseSchedule, usesTimeZone } from '@/lib/backupSchedule';
import { scheduleIntervalMs } from '@/lib/platformBackupSchedule';
import { setTimePrefs } from '@/lib/time';

const h24: Intl.DateTimeFormatOptions = { hourCycle: 'h23' };
const h12: Intl.DateTimeFormatOptions = { hour12: true };

describe('parseSchedule / buildSchedule', () => {
    it('round-trips every form Core accepts', () => {
        for (const s of [
            'manual', 'every 6h', 'every 1d', 'every 7d', 'every 6h from 12:00', 'every 1h from 00:15',
            'daily 03:00', 'weekly sun 23:59', 'cron 0 */6 * * *', 'cron 30 2 * * 1-5',
        ]) {
            expect(buildSchedule(parseSchedule(s))).toBe(s);
        }
    });

    it('reads the fields out of a stored schedule', () => {
        expect(parseSchedule('every 6h from 14:30')).toMatchObject({ kind: 'anchored', every: 6, time: '14:30' });
        expect(parseSchedule('every 12d')).toMatchObject({ kind: 'interval', every: 12, unit: 'd' });
        expect(parseSchedule('weekly fri 4:05')).toMatchObject({ kind: 'weekly', weekday: 'fri', time: '04:05' });
        expect(parseSchedule('')).toMatchObject({ kind: 'manual' });
    });

    it('keeps a value it cannot read exactly as written', () => {
        // Saved before validation got strict; saving the form must not rewrite it.
        expect(buildSchedule(parseSchedule('every 6h at 12:00'))).toBe('every 6h at 12:00');
        expect(parseSchedule('daily 25:00').kind).toBe('cron');
    });

    it('only wall-clock forms read the zone', () => {
        expect(usesTimeZone('manual')).toBe(false);
        expect(usesTimeZone('interval')).toBe(false);
        expect(usesTimeZone('anchored')).toBe(true);
        expect(usesTimeZone('cron')).toBe(true);
    });
});

describe('anchoredTimes', () => {
    it('walks the day from the anchor, earliest first', () => {
        expect(anchoredTimes(6, '12:00')).toEqual(['00:00', '06:00', '12:00', '18:00']);
        expect(anchoredTimes(8, '14:30')).toEqual(['06:30', '14:30', '22:30']);
        expect(anchoredTimes(24, '03:15')).toEqual(['03:15']);
        expect(anchoredTimes(5, '12:00')).toEqual([]);
    });
});

describe('fmtClock / describeSchedule', () => {
    it('shows a wall time in the viewer clock format', () => {
        expect(fmtClock('14:30', h24)).toMatch(/14:30/);
        expect(fmtClock('14:30', h12)).toMatch(/2:30\s*PM/i);
        expect(fmtClock('00:05', h12)).toMatch(/12:05\s*AM/i);
    });

    it('says the schedule in words', () => {
        expect(describeSchedule('manual')).toBe('Manual only');
        expect(describeSchedule('every 1h')).toBe('Every 1 hour');
        expect(describeSchedule('every 2d', 'Europe/Berlin')).toBe('Every 2 days');
        expect(describeSchedule('every 6h from 12:00', 'Europe/Berlin', h24)).toMatch(/^Every 6 hours, at 00:00, 06:00, 12:00, 18:00 \(Europe\/Berlin\)$/);
        expect(describeSchedule('every 6h from 12:00', 'UTC', h12)).toMatch(/12:00\s*AM, 6:00\s*AM, 12:00\s*PM, 6:00\s*PM/);
        expect(describeSchedule('every 1h from 00:00', 'UTC', h24)).toMatch(/00:00, 01:00, 02:00, 03:00, \.\.\./);
        expect(describeSchedule('daily 03:00', 'America/New_York', h24)).toBe('Daily at 03:00 (America/New York)');
        expect(describeSchedule('weekly mon 04:15', 'UTC', h24)).toBe('Mondays at 04:15 (UTC)');
        expect(describeSchedule('cron 0 3 * * 1-5', 'UTC')).toBe('cron 0 3 * * 1-5 (UTC)');
    });
});

describe('editorZone', () => {
    it('keeps the zone of a wall-clock job and offers the viewer zone otherwise', () => {
        setTimePrefs({ timezone: 'Asia/Tokyo', timeFormat: 'auto' });
        expect(editorZone('daily 03:00', 'Europe/Berlin')).toBe('Europe/Berlin');
        // A relative job stored UTC by default; nobody chose that.
        expect(editorZone('every 6h', 'UTC')).toBe('Asia/Tokyo');
        expect(editorZone('manual')).toBe('Asia/Tokyo');
        setTimePrefs({ timezone: '', timeFormat: 'auto' });
    });
});

describe('scheduleIntervalMs on the new forms', () => {
    it('knows the spacing of every form except cron', () => {
        const hour = 3_600_000;
        expect(scheduleIntervalMs('every 6h from 12:00')).toBe(6 * hour);
        expect(scheduleIntervalMs('daily 03:00')).toBe(24 * hour);
        expect(scheduleIntervalMs('weekly mon 03:00')).toBe(7 * 24 * hour);
        expect(scheduleIntervalMs('cron 0 3 * * *')).toBeNull();
    });
});
