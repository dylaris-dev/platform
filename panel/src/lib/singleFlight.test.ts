import { describe, it, expect } from 'vitest';
import { singleFlight } from './singleFlight';

describe('singleFlight', () => {
    it('runs a burst of calls as one run plus one trailing run', async () => {
        let runs = 0;
        let release: () => void = () => {};
        const refresh = singleFlight(() => {
            runs++;
            return new Promise<void>(r => { release = r; });
        });
        const first = refresh();
        const calls = [refresh(), refresh(), refresh()];
        expect(runs).toBe(1);
        release();
        await new Promise(r => setTimeout(r, 0));
        expect(runs).toBe(2); // the trailing run, for the calls made meanwhile
        release();
        await Promise.all([first, ...calls]);
        expect(runs).toBe(2);
        const later = refresh(); // after the run ended: a new run
        expect(runs).toBe(3);
        release();
        await later;
    });

    it('still runs the pass a caller is owed when the earlier pass fails', async () => {
        let runs = 0;
        let fail: (e: Error) => void = () => {};
        const refresh = singleFlight(() => {
            runs++;
            return runs === 1 ? new Promise<void>((_, rej) => { fail = rej; }) : Promise.resolve();
        });
        const first = refresh();
        const owed = refresh();
        fail(new Error('network'));
        await expect(owed).resolves.toBeUndefined();
        await expect(first).resolves.toBeUndefined();
        expect(runs).toBe(2);
    });

    it('lets a failure reach the caller and does not wedge later calls', async () => {
        let fail = true;
        const refresh = singleFlight(async () => { if (fail) throw new Error('down'); });
        await expect(refresh()).rejects.toThrow('down');
        fail = false;
        await expect(refresh()).resolves.toBeUndefined();
    });
});
