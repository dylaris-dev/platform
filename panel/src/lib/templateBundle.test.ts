import { describe, expect, it } from 'vitest';
import { bundleFiles, retryableFailure } from './templateBundle';

const f = (path: string, size = 1) => ({ path, size });

describe('bundleFiles', () => {
    it('returns no batches for no files', () => {
        expect(bundleFiles([])).toEqual([]);
    });

    it('splits by count', () => {
        const files = Array.from({ length: 5 }, (_, i) => f(`a${i}`));
        expect(bundleFiles(files, 2).map((b) => b.length)).toEqual([2, 2, 1]);
    });

    it('splits by bytes and keeps order', () => {
        const batches = bundleFiles([f('a', 60), f('b', 60), f('c', 30)], 200, 100);
        expect(batches.map((b) => b.map((x) => x.path))).toEqual([['a'], ['b', 'c']]);
    });

    it('gives a file larger than the byte cap its own batch', () => {
        const batches = bundleFiles([f('a', 10), f('big', 500), f('c', 10)], 200, 100);
        expect(batches.map((b) => b.map((x) => x.path))).toEqual([['a'], ['big'], ['c']]);
    });

    it('never produces an empty batch', () => {
        const batches = bundleFiles([f('big', 500), f('big2', 500), f('z', 0)], 1, 100);
        expect(batches.every((b) => b.length > 0)).toBe(true);
        expect(batches.flat()).toHaveLength(3);
    });
});

describe('retryableFailure', () => {
    it('retries network errors and 5xx, not 4xx', () => {
        expect(retryableFailure(undefined)).toBe(true);
        expect(retryableFailure({ success: false })).toBe(true);
        expect(retryableFailure({ success: false, status: 502 })).toBe(true);
        expect(retryableFailure({ success: false, status: 413 })).toBe(false);
        expect(retryableFailure({ success: false, status: 400 })).toBe(false);
    });
});
