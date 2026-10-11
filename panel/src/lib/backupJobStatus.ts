// What the backups page says about a job's schedule.

/**
 * The skip to warn about, or null. Core records the newest scheduled slot that
 * did not run; it only matters while no run has happened since, so a skip
 * followed by a successful run is history, not a warning.
 */
export function currentSkip(job: {
    lastRunAt?: string | null;
    lastSkipAt?: string | null;
    lastSkipReason?: string | null;
}): { at: string; reason: string } | null {
    if (!job.lastSkipAt) return null;
    const skip = new Date(job.lastSkipAt).getTime();
    if (Number.isNaN(skip)) return null;
    if (job.lastRunAt) {
        const run = new Date(job.lastRunAt).getTime();
        if (!Number.isNaN(run) && run >= skip) return null;
    }
    return { at: job.lastSkipAt, reason: job.lastSkipReason || 'no reason recorded' };
}

/** "5m ago", "in 3h", "just now": relative in both directions. */
export function relativeTime(iso: string | null | undefined, now: number = Date.now()): string {
    if (!iso) return '—';
    const t = new Date(iso).getTime();
    if (Number.isNaN(t)) return '—';
    const diff = t - now;
    const m = Math.floor(Math.abs(diff) / 60000);
    if (m < 1) return diff > 0 ? 'in under a minute' : 'just now';
    const h = Math.floor(m / 60);
    const span = m < 60 ? `${m}m` : h < 24 ? `${h}h` : `${Math.floor(h / 24)}d`;
    return diff > 0 ? `in ${span}` : `${span} ago`;
}
