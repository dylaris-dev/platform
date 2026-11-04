import { describe, expect, it } from 'vitest';
import { suggestedCap } from './speedTest';

describe('suggestedCap', () => {
    it.each([
        [1000, 700],
        [412, 288],
        [1, 1], // never 0: a 0 Mbit/s cap would hang every backup
        [0, 1],
    ])('%s Mbit/s measured -> %s', (mbit, want) => {
        expect(suggestedCap(mbit)).toBe(want);
    });
});
