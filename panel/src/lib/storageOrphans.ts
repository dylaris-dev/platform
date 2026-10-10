import type { LifecycleRule, OrphanCandidate, OrphanDeleteResult, OrphanScan } from '@/lib/api/storageMaintenance';

/** Storage providers that are an S3/R2 bucket the maintenance tools can reach. */
export function supportsStorageMaintenance(provider: string): boolean {
    return provider === 's3' || provider === 'connection' || provider === 'core-storage';
}

/** Count and bytes of the selected candidates. */
export function selectionTotals(candidates: OrphanCandidate[], selected: ReadonlySet<string>): { count: number; bytes: number } {
    let count = 0;
    let bytes = 0;
    for (const c of candidates) {
        if (selected.has(c.key)) {
            count++;
            bytes += c.size;
        }
    }
    return { count, bytes };
}

/** "Select all" when not everything is selected, otherwise clear. */
export function toggleAll(candidates: OrphanCandidate[], selected: ReadonlySet<string>): Set<string> {
    const all = candidates.length > 0 && candidates.every(c => selected.has(c.key));
    return all ? new Set() : new Set(candidates.map(c => c.key));
}

/** Coarse age of a file: hours under two days, days after. */
export function orphanAge(lastModified: string, now: number = Date.now()): string {
    const ms = now - new Date(lastModified).getTime();
    if (!Number.isFinite(ms) || ms < 0) return '-';
    const hours = Math.floor(ms / 3_600_000);
    return hours < 48 ? `${hours} h` : `${Math.floor(hours / 24)} d`;
}

/** Keeps the start and the end of a long key, which are the parts that identify it. */
export function truncateKey(key: string, max = 56): string {
    if (key.length <= max) return key;
    const keep = max - 3;
    const head = Math.ceil(keep / 2);
    return `${key.slice(0, head)}...${key.slice(key.length - (keep - head))}`;
}

/** What a delete request achieved, for one toast. */
export function summarizeDelete(results: OrphanDeleteResult[]): { deleted: number; bytes: number; refused: number; firstReason?: string } {
    let deleted = 0;
    let bytes = 0;
    let refused = 0;
    let firstReason: string | undefined;
    for (const r of results) {
        if (r.deleted) {
            deleted++;
            bytes += r.size ?? 0;
        } else {
            refused++;
            firstReason ??= r.reason;
        }
    }
    return { deleted, bytes, refused, firstReason };
}

/** One rule as a sentence. */
export function describeLifecycleRule(r: LifecycleRule): string {
    const scope = r.prefix ? `under ${r.prefix}` : 'whole bucket';
    const parts: string[] = [];
    if (r.abortMultipartDays) parts.push(`abort unfinished uploads after ${r.abortMultipartDays} d`);
    if (r.expirationDays) parts.push(`delete files after ${r.expirationDays} d`);
    if (parts.length === 0) parts.push('other action');
    return `${parts.join(', ')} (${scope})`;
}

/** Whether every rule Core plans is present with the same settings. */
export function lifecycleUpToDate(current: LifecycleRule[], planned: LifecycleRule[]): boolean {
    return planned.every(p => current.some(c =>
        c.id === p.id && c.status === p.status && c.prefix === p.prefix
        && (c.expirationDays ?? 0) === (p.expirationDays ?? 0)
        && (c.abortMultipartDays ?? 0) === (p.abortMultipartDays ?? 0)));
}

/** The scan with every deleted file taken out of both lists and their totals. */
export function dropDeleted(scan: OrphanScan, results: OrphanDeleteResult[]): OrphanScan {
    const gone = new Set(results.filter(r => r.deleted).map(r => r.key));
    const keep = (rows: OrphanCandidate[]) => rows.filter(c => !gone.has(c.key));
    const bytes = (rows: OrphanCandidate[]) => rows.reduce((n, c) => n + c.size, 0);
    const candidates = keep(scan.candidates);
    const live = keep(scan.live ?? []);
    return { ...scan, candidates, candidateBytes: bytes(candidates), live, liveBytes: bytes(live) };
}
