/**
 * How fresh the live players figure is, in words.
 *
 * "Live" only while the last successful poll is recent. A poll that keeps
 * failing leaves the last number on screen, and without this the number would
 * keep reading as current long after it stopped being one.
 */
export const LIVE_FOR_MS = 15_000;

export function liveStatus(at: number | null, now: number): { live: boolean; label: string } {
    if (at === null) return { live: false, label: 'Waiting for the first reading' };
    const age = Math.max(0, now - at);
    if (age <= LIVE_FOR_MS) return { live: true, label: 'Live' };
    const s = Math.round(age / 1000);
    return { live: false, label: s < 120 ? `Updated ${s}s ago` : `Updated ${Math.round(s / 60)} min ago` };
}
