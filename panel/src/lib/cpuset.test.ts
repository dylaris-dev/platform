import { describe, it, expect } from 'vitest';
import { parseCpuset, compactCpuset, pinTooSmall } from './cpuset';

describe('parseCpuset', () => {
    it('parses a single id', () => {
        expect(parseCpuset('5')).toEqual([5]);
    });

    it('parses a simple range', () => {
        expect(parseCpuset('0-3')).toEqual([0, 1, 2, 3]);
    });

    it('parses a mix of ranges and singles, de-duplicated and sorted', () => {
        expect(parseCpuset('8,0-3,2')).toEqual([0, 1, 2, 3, 8]);
    });

    it('ignores whitespace around tokens', () => {
        expect(parseCpuset(' 0 , 2-4 , 9 ')).toEqual([0, 2, 3, 4, 9]);
    });

    it('ignores empty segments from stray commas', () => {
        expect(parseCpuset('1,,3')).toEqual([1, 3]);
    });

    it('ignores a malformed range where lo > hi', () => {
        expect(parseCpuset('5-2')).toEqual([]);
    });

    it('ignores non-numeric tokens', () => {
        expect(parseCpuset('abc,1')).toEqual([1]);
    });

    it('returns an empty array for an empty string', () => {
        expect(parseCpuset('')).toEqual([]);
    });
});

describe('compactCpuset', () => {
    it('compacts a contiguous range', () => {
        expect(compactCpuset([0, 1, 2, 3])).toBe('0-3');
    });

    it('compacts mixed contiguous and isolated ids', () => {
        expect(compactCpuset([0, 1, 2, 3, 8])).toBe('0-3,8');
    });

    it('de-duplicates and sorts unordered input', () => {
        expect(compactCpuset([3, 1, 1, 2, 0])).toBe('0-3');
    });

    it('renders a single id without a range', () => {
        expect(compactCpuset([5])).toBe('5');
    });

    it('returns an empty string for an empty array', () => {
        expect(compactCpuset([])).toBe('');
    });

    it('round-trips through parseCpuset', () => {
        const spec = '0-3,8,10-12';
        expect(compactCpuset(parseCpuset(spec))).toBe(spec);
    });
});

describe('pinTooSmall', () => {
    it('refuses a manual pin with fewer cores than the limit grants', () => {
        expect(pinTooSmall(4, 'manual', '0-2')).toEqual({ have: 3, need: 4 });
        expect(pinTooSmall(2.5, 'manual', '0-1')).toEqual({ have: 2, need: 3 });
    });
    it('accepts a pin at or above the limit, no limit, no pin, or other modes', () => {
        expect(pinTooSmall(4, 'manual', '0-3')).toBeNull();
        expect(pinTooSmall(4, 'manual', '0-7')).toBeNull();
        expect(pinTooSmall(0, 'manual', '0')).toBeNull();
        expect(pinTooSmall(4, 'manual', '')).toBeNull();
        expect(pinTooSmall(4, 'auto', '0')).toBeNull();
    });
});
