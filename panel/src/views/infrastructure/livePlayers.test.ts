import { describe, expect, it } from 'vitest';
import { LIVE_FOR_MS, liveStatus } from './livePlayers';

describe('live players freshness', () => {
    const now = 1_800_000_000_000;

    it('has nothing to call live before the first reading', () => {
        expect(liveStatus(null, now)).toEqual({ live: false, label: 'Waiting for the first reading' });
    });

    it('is live while the last reading is recent', () => {
        expect(liveStatus(now - 4_000, now)).toEqual({ live: true, label: 'Live' });
        expect(liveStatus(now - LIVE_FOR_MS, now).live).toBe(true);
    });

    it('stops claiming live once polls stop succeeding', () => {
        expect(liveStatus(now - 40_000, now)).toEqual({ live: false, label: 'Updated 40s ago' });
        expect(liveStatus(now - 5 * 60_000, now)).toEqual({ live: false, label: 'Updated 5 min ago' });
    });
});
