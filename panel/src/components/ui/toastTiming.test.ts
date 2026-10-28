import { describe, expect, it } from 'vitest';
import { advanceToastClock, toastDuration, toastOpacity } from './toastTiming';

describe('toast lifetime', () => {
    it('shortens existing success, error and extended lifetimes by 35 percent', () => {
        expect(toastDuration(true)).toBe(5200);
        expect(toastDuration(false)).toBe(9100);
        expect(toastDuration(false, 20000)).toBe(13000);
    });
    it('preserves time spent before multiple hover/focus pauses', () => {
        let clock = { elapsed: 0, at: 0, paused: false };
        clock = advanceToastClock(clock, 2000, 5200, true);
        clock = advanceToastClock(clock, 12000, 5200, false);
        expect(clock.elapsed).toBe(2000);
        clock = advanceToastClock(clock, 14000, 5200, true);
        clock = advanceToastClock(clock, 19000, 5200, false);
        expect(clock.elapsed).toBe(4000);
        expect(advanceToastClock(clock, 20200, 5200).elapsed).toBe(5200);
    });
    it('fades only during the second half and bounds clock overshoot', () => {
        expect(toastOpacity(2500, 5000)).toBe(1);
        expect(toastOpacity(3750, 5000)).toBeCloseTo(0.625);
        expect(toastOpacity(5000, 5000)).toBe(0.25);
        expect(advanceToastClock({ elapsed: 4000, at: 0, paused: false }, 2000, 5000).elapsed).toBe(5000);
    });
});
