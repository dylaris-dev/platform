import { describe, it, expect } from 'vitest';
import { formatBytes, formatMB } from './format';

describe('formatBytes', () => {
    it.each([
        [0, '0 B'],
        [-5, '0 B'],
        [Number.NaN, '0 B'],
        [512, '512 B'],
        [1536, '1.5 KB'],
        [1024 ** 2, '1 MB'],
        [45.3 * 1024 ** 3, '45.3 GB'],
        [120 * 1024 ** 3, '120 GB'],
        [2 * 1024 ** 4, '2 TB'],
    ])('%s -> %s', (n, want) => {
        expect(formatBytes(n)).toBe(want);
    });
});

describe('formatMB', () => {
    // The admin servers list showed "0 MB" for this: an MB value read as bytes.
    it('reads a disk limit stored in MB as the GB it was typed as', () => {
        expect(formatMB(81920)).toBe('80 GB');
    });

    it('keeps RAM under a GB in MB', () => {
        expect(formatMB(512)).toBe('512 MB');
        expect(formatMB(1536)).toBe('1.5 GB');
    });
});
