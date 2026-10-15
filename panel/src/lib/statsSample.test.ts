import { describe, it, expect } from 'vitest';
import { parseStatsSample } from './statsSample';

describe('parseStatsSample', () => {
    it('reads a sample', () => {
        expect(parseStatsSample('{"ts":1,"cpu":2.5,"memUsed":900,"memLimit":1536}')?.cpu).toBe(2.5);
    });

    it('drops the disk warning the node sends on the same channel', () => {
        expect(parseStatsSample('{"type":"disk_warning","usedMb":900,"limitMb":1000}')).toBeNull();
    });

    it('drops garbage', () => {
        expect(parseStatsSample('not json')).toBeNull();
        expect(parseStatsSample('null')).toBeNull();
        expect(parseStatsSample('{"memUsed":900}')).toBeNull();
        expect(parseStatsSample('{"cpu":1.5}')).toBeNull();
    });
});
