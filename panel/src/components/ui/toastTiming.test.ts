import { describe, expect, it } from 'vitest';
import { toastDuration } from './toastTiming';

describe('toast lifetime', () => {
    it('shortens existing success, error and extended lifetimes by 35 percent', () => {
        expect(toastDuration(true)).toBe(5200);
        expect(toastDuration(false)).toBe(9100);
        expect(toastDuration(false, 20000)).toBe(13000);
    });
});
