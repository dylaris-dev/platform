// The panel's one byte convention: 1024-based, labelled KB/MB/GB/TB.
//
// 1024 rather than 1000 because every size the platform is CONFIGURED in is
// 1024-based: a disk limit typed as 80 GB is stored as 81920 MB, the backup
// quota is quotaGB * 1024^3 and RAM is booked in MB. A 1000-based display
// would show that 80 GB limit as "85.9 GB" next to the field that says 80, and
// a server at its limit as over it. The labels match the input fields, which
// say GB and MB.

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];

/** A byte count for display: "512 B", "1.5 MB", "80 GB", "120 GB". */
export function formatBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    let value = bytes;
    let unit = 0;
    while (value >= 1024 && unit < UNITS.length - 1) {
        value /= 1024;
        unit += 1;
    }
    if (unit === 0) return `${Math.round(value)} B`;
    // One decimal below 100 so 45.3 GB stays readable; ".0" dropped so a round
    // limit reads as the number that was typed.
    const shown = value >= 100 ? String(Math.round(value)) : value.toFixed(1).replace(/\.0$/, '');
    return `${shown} ${UNITS[unit]}`;
}

/**
 * A byte count for display in DECIMAL units (1 KB = 1000 B). Only for amounts
 * someone else counts that way - the gateway provider sells traffic in decimal
 * GB, where 5000 GB is 5 TB - so the number matches the plan that was bought.
 */
export function formatDecimalBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    let value = bytes;
    let unit = 0;
    while (value >= 1000 && unit < UNITS.length - 1) {
        value /= 1000;
        unit += 1;
    }
    if (unit === 0) return `${Math.round(value)} B`;
    const shown = value >= 100 ? String(Math.round(value)) : value.toFixed(1).replace(/\.0$/, '');
    return `${shown} ${UNITS[unit]}`;
}

/** A size the API carries in MB (RAM, disk limits). */
export function formatMB(mb: number): string {
    return formatBytes(mb * 1024 * 1024);
}
