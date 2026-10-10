import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createCoalescer } from '@/lib/coalesce';

describe('createCoalescer', () => {
    beforeEach(() => { vi.useFakeTimers(); });
    afterEach(() => { vi.useRealTimers(); });

    it('collapses a burst for one key into a single run', async () => {
        const run = createCoalescer(300);
        const fn = vi.fn(async () => {});
        for (let i = 0; i < 60; i++) run(7, fn);
        await vi.advanceTimersByTimeAsync(299);
        expect(fn).not.toHaveBeenCalled();
        await vi.advanceTimersByTimeAsync(1);
        expect(fn).toHaveBeenCalledTimes(1);
    });

    it('keeps keys apart', async () => {
        const run = createCoalescer(300);
        const a = vi.fn(async () => {});
        const b = vi.fn(async () => {});
        run(1, a); run(2, b); run(1, a);
        await vi.advanceTimersByTimeAsync(300);
        expect(a).toHaveBeenCalledTimes(1);
        expect(b).toHaveBeenCalledTimes(1);
    });

    it('never overlaps runs and queues exactly one more for calls made in flight', async () => {
        const run = createCoalescer(300);
        let inFlight = 0;
        let maxInFlight = 0;
        let release!: () => void;
        const fn = vi.fn(() => {
            inFlight++;
            maxInFlight = Math.max(maxInFlight, inFlight);
            return new Promise<void>(r => { release = () => { inFlight--; r(); }; });
        });
        run(7, fn);
        await vi.advanceTimersByTimeAsync(300);
        expect(fn).toHaveBeenCalledTimes(1);

        // Three changes land while the first read is still running.
        run(7, fn); run(7, fn);
        await vi.advanceTimersByTimeAsync(300);
        run(7, fn);
        await vi.advanceTimersByTimeAsync(300);
        expect(fn).toHaveBeenCalledTimes(1);

        release();
        await vi.advanceTimersByTimeAsync(300);
        expect(fn).toHaveBeenCalledTimes(2);
        release();
        await vi.advanceTimersByTimeAsync(1000);
        expect(fn).toHaveBeenCalledTimes(2);
        expect(maxInFlight).toBe(1);
    });

    it('runs again after a failed run', async () => {
        const run = createCoalescer(300);
        const fn = vi.fn(async () => { throw new Error('boom'); });
        run(7, fn);
        await vi.advanceTimersByTimeAsync(300);
        run(7, fn);
        await vi.advanceTimersByTimeAsync(300);
        expect(fn).toHaveBeenCalledTimes(2);
    });
});
