import { describe, it, expect } from 'vitest';
import { jvmHeapMB } from './jvmHeap';

describe('jvmHeapMB', () => {
    // The same table as the node's; a drift here shows the wrong -Xmx.
    it.each([
        [512, 512], [1024, 1024], [2048, 2048], [2049, 2048], [2560, 2048],
        [3072, 2560], [4096, 3482], [12288, 10445], [20480, 18432],
    ])('%i MB booked -> %i MB heap', (booked, heap) => {
        expect(jvmHeapMB(booked)).toBe(heap);
    });
});
