import type { PlatformBackupJob } from '@/lib/api/platformBackups';
import { parseSchedule } from '@/lib/backupSchedule';

/**
 * The longest gap between two runs in ms; null for manual, for cron (which
 * needs Core's parser to tell) and for anything unparseable.
 */
export function scheduleIntervalMs(schedule: string): number | null {
    const d = parseSchedule(schedule);
    const hour = 3_600_000;
    if (d.every <= 0) return null;
    switch (d.kind) {
        case 'interval': return d.every * (d.unit === 'd' ? 24 : 1) * hour;
        case 'anchored': return 24 % d.every === 0 ? d.every * hour : null;
        case 'daily': return 24 * hour;
        case 'weekly': return 7 * 24 * hour;
        default: return null;
    }
}

/**
 * Why an enabled, scheduled job is not running on its schedule, or null when
 * it is. A job with no next run was never armed (production job 3 sat like
 * that for eleven days); one more than twice its interval past its next run
 * means the scheduler is not picking it up.
 */
export function scheduleWarning(job: Pick<PlatformBackupJob, 'enabled' | 'schedule' | 'nextRunAt'>, now: number = Date.now()): string | null {
    const interval = scheduleIntervalMs(job.schedule);
    if (!job.enabled || interval === null) return null;
    if (!job.nextRunAt) return 'No next run is scheduled, so this job does not run on its schedule.';
    const overdue = now - new Date(job.nextRunAt).getTime();
    if (overdue > 2 * interval) return 'The scheduled run is overdue by more than twice its interval.';
    return null;
}
