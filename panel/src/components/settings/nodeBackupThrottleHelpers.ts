import type { NodeBackupThrottle } from '@/lib/api';

/**
 * LimitField hands back 0 when its "no limit" switch is turned off, because 0
 * is a real cap everywhere else. For a rate it is not: 0 B/s would hang the
 * backup, and Core refuses it. Switching the cap on starts at a usable value.
 */
export function withRateFloor(v: number | null, start: number): number | null {
    if (v === null) return null;
    return v < 1 ? start : v;
}

export function throttleChanged(a: NodeBackupThrottle, b: NodeBackupThrottle): boolean {
    return a.uplinkMbit !== b.uplinkMbit
        || a.readMBps !== b.readMBps
        || a.spoolEnabled !== b.spoolEnabled
        || a.spoolMaxGB !== b.spoolMaxGB;
}
