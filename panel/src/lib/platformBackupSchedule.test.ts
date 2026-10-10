import { describe, expect, it } from 'vitest';
import { scheduleIntervalMs, scheduleWarning } from '@/lib/platformBackupSchedule';

const now = Date.parse('2026-10-10T12:00:00Z');
const day = 24 * 3_600_000;

describe('scheduleIntervalMs', () => {
    it('parses the forms the scheduler understands', () => {
        expect(scheduleIntervalMs('every 1d')).toBe(day);
        expect(scheduleIntervalMs('every 6h')).toBe(6 * 3_600_000);
        expect(scheduleIntervalMs('manual')).toBeNull();
        expect(scheduleIntervalMs('daily')).toBeNull();
        expect(scheduleIntervalMs('every 0h')).toBeNull();
    });
});

describe('scheduleWarning', () => {
    it('warns about an enabled scheduled job that was never armed', () => {
        // Production job 3: every 1d, enabled, next_run_at NULL.
        expect(scheduleWarning({ enabled: true, schedule: 'every 1d' }, now)).toMatch(/No next run/);
    });

    it('warns once a run is more than twice its interval overdue', () => {
        const at = (ms: number) => new Date(now - ms).toISOString();
        expect(scheduleWarning({ enabled: true, schedule: 'every 1d', nextRunAt: at(2 * day + 60_000) }, now)).toMatch(/overdue/);
        expect(scheduleWarning({ enabled: true, schedule: 'every 1d', nextRunAt: at(day) }, now)).toBeNull();
        expect(scheduleWarning({ enabled: true, schedule: 'every 1d', nextRunAt: new Date(now + day).toISOString() }, now)).toBeNull();
    });

    it('stays quiet for manual and disabled jobs', () => {
        expect(scheduleWarning({ enabled: true, schedule: 'manual' }, now)).toBeNull();
        expect(scheduleWarning({ enabled: false, schedule: 'every 1d' }, now)).toBeNull();
    });
});
