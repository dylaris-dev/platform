import { describe, it, expect } from 'vitest';
import { sliderPosition, sliderStops } from './resourceSlider';

const disk = { min: 10, step: 10, max: 100, unlimitedAtEnd: true };
const ram = { min: 512, step: 512, max: 2048 };

describe('sliderStops', () => {
    it('ends disk at 100 GB, then unlimited', () => {
        expect(sliderStops(disk, 20)).toEqual([10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 0]);
    });

    it('stops widening at four times max', () => {
        expect(sliderStops(ram, 1e9).length).toBe(16);
    });

    it('widens the scale to a value above max', () => {
        expect(sliderStops(ram, 3000)).toEqual([512, 1024, 1536, 2048, 2560, 3072]);
    });
});

describe('sliderPosition', () => {
    it('puts unlimited on the last stop', () => {
        const stops = sliderStops(disk, 0);
        expect(sliderPosition(stops, 0, true)).toBe(stops.length - 1);
    });

    it('never lands a small value on the unlimited stop', () => {
        // 4 GB is nearer to the unlimited stop's 0 than to 10.
        const stops = sliderStops(disk, 4);
        expect(stops[sliderPosition(stops, 4, true)]).toBe(10);
    });

    it('picks the nearest stop for a value between stops', () => {
        const stops = sliderStops(ram, 1100);
        expect(stops[sliderPosition(stops, 1100)]).toBe(1024);
    });

    it('keeps the top capped stop for a value at max', () => {
        const stops = sliderStops(disk, 100);
        expect(stops[sliderPosition(stops, 100, true)]).toBe(100);
    });
});
