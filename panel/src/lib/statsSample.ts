import type { ServerStats } from './api/types';

/**
 * One message off the stats stream, or null when it is not a sample: the node
 * publishes its disk warnings on the same channel, and a warning read as a
 * sample has no cpu to draw.
 */
export function parseStatsSample(data: string): ServerStats | null {
    let s: unknown;
    try { s = JSON.parse(data); } catch { return null; }
    if (!s || typeof s !== 'object') return null;
    const o = s as Partial<ServerStats>;
    return typeof o.cpu === 'number' && typeof o.memUsed === 'number' ? (o as ServerStats) : null;
}
