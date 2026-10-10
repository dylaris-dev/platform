import type { PlatformBackupJob } from '@/lib/api/platformBackups';

/** The interval of an "every <n>h" / "every <n>d" schedule in ms; null for manual or unparseable. */
export function scheduleIntervalMs(schedule: string): number | null {
    const m = /^every (\d+)([hd])$/.exec(schedule.trim());
    if (!m || Number(m[1]) <= 0) return null;
    return Number(m[1]) * (m[2] === 'd' ? 24 : 1) * 3_600_000;
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
