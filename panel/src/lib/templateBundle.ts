/** Mirrors Core's limits for one add-files request (maxBundleFiles / maxBundleBytes). */
export const BUNDLE_MAX_FILES = 200;
export const BUNDLE_MAX_BYTES = 100 * 1024 * 1024;

/**
 * Splits files into upload batches, in order. A single file over maxBytes forms
 * its own batch: Core then decides against the template size cap. No batch is empty.
 */
export function bundleFiles<T extends { size: number }>(
    files: T[],
    maxFiles = BUNDLE_MAX_FILES,
    maxBytes = BUNDLE_MAX_BYTES,
): T[][] {
    const batches: T[][] = [];
    let cur: T[] = [];
    let bytes = 0;
    for (const f of files) {
        if (cur.length > 0 && (cur.length >= maxFiles || bytes + f.size > maxBytes)) {
            batches.push(cur);
            cur = [];
            bytes = 0;
        }
        cur.push(f);
        bytes += f.size;
    }
    if (cur.length > 0) batches.push(cur);
    return batches;
}

/** A failed batch is worth one more try only when the failure may be transient: no HTTP answer, or a 5xx. */
export const retryableFailure = (res?: { success: boolean; status?: number }) =>
    !res || (!res.success && (res.status === undefined || res.status >= 500));
