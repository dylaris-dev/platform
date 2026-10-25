// Pure helpers for the extra ports card (server page) and the admin card.

/** What a player or tool connects to. Without a node address the bare port
 *  is still worth showing. */
export function portAddress(address: string, hostPort: number): string {
    return address ? `${address}:${hostPort}` : String(hostPort);
}

/** "1 / 2", "1 / no limit", "0 / none". */
export function portUsage(used: number, limit: number | null): string {
    if (limit === null) return `${used} / no limit`;
    if (limit === 0) return `${used} / none`;
    return `${used} / ${limit}`;
}

/** The settings value ("unlimited" or a number as text) as a LimitField
 *  value. Anything unreadable is the product default, 2, which is also what
 *  Core reads for it. */
export const DEFAULT_PORTS_PER_USER = 2;
export function parseMaxPerUser(v: string): number | null {
    if (v.trim() === 'unlimited') return null;
    const n = Number(v);
    return v.trim() !== '' && Number.isInteger(n) && n >= 0 ? n : DEFAULT_PORTS_PER_USER;
}
export function formatMaxPerUser(v: number | null): string {
    return v === null ? 'unlimited' : String(v);
}

/** Core's sentence first: every refusal it sends is written for the user. */
export function portsError(res: { error?: string; message?: string } | null | undefined, fallback: string): string {
    return res?.error || res?.message || fallback;
}

/** A host port as typed by an admin: "" = assign from the range. */
export function parseHostPort(v: string): number | undefined | 'invalid' {
    const t = v.trim();
    if (t === '') return undefined;
    const n = Number(t);
    return Number.isInteger(n) && n >= 1024 && n <= 65535 ? n : 'invalid';
}

/** A rate in bytes per second as network people read it: bits. */
export function formatBitRate(bytesPerSec: number): string {
    const bits = Math.max(0, bytesPerSec) * 8;
    if (bits >= 1e9) return `${(bits / 1e9).toFixed(1)} Gbit/s`;
    if (bits >= 1e6) return `${(bits / 1e6).toFixed(1)} Mbit/s`;
    if (bits >= 1e3) return `${(bits / 1e3).toFixed(0)} kbit/s`;
    return `${bits.toFixed(0)} bit/s`;
}

