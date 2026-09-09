import { describe, it, expect } from 'vitest';
import { cacheSuccess } from './cacheSuccess';

// The Beam app's session handshake cached the ATTEMPT rather than the RESULT,
// so one failure answered every later caller for the rest of the app run: the
// native side stayed unauthenticated and every file operation on every server
// came back "not logged in", with nothing left that would retry.
//
// These pin the difference between "it is running or it worked" and "somebody
// tried once".

function deferred<T>() {
    let resolve!: (v: T) => void;
    let reject!: (e: unknown) => void;
    const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
}

const ok = (r: { ok: boolean }) => r.ok;

describe('cacheSuccess', () => {
    it('runs the step once when it succeeds', async () => {
        let runs = 0;
        const step = cacheSuccess(async () => { runs++; return { ok: true }; }, ok);

        await step();
        await step();
        await step();

        expect(runs).toBe(1);
    });

    it('runs it again after a failure', async () => {
        // The defect. A failed handshake must not become the permanent answer.
        let runs = 0;
        const step = cacheSuccess(async () => { runs++; return { ok: false }; }, ok);

        await step();
        await step();

        expect(runs).toBe(2);
    });

    it('stops running it once a retry finally works', async () => {
        let runs = 0;
        const step = cacheSuccess(async () => { runs++; return { ok: runs > 2 }; }, ok);

        await step();
        await step();
        await step();
        await step();

        expect(runs).toBe(3);
    });

    it('treats a rejection as a failure and lets it through', async () => {
        let runs = 0;
        const step = cacheSuccess<{ ok: boolean }>(async () => {
            runs++;
            throw new Error('boom');
        }, ok);

        await expect(step()).rejects.toThrow('boom');
        await expect(step()).rejects.toThrow('boom');
        expect(runs).toBe(2);
    });

    it('gives concurrent callers the same attempt', async () => {
        // Not an optimisation. The step this guards tears down a live tunnel,
        // so two at once is not the same as two in a row.
        let runs = 0;
        const gate = deferred<void>();
        const step = cacheSuccess(async () => { runs++; await gate.promise; return { ok: true }; }, ok);

        const a = step();
        const b = step();
        expect(runs).toBe(1);
        expect(a).toBe(b);

        gate.resolve();
        await a;
        await b;
        expect(runs).toBe(1);
    });

    it('shares the RETRY too, not only the first attempt', async () => {
        // The sharing has to survive a failure. If a retry were per-caller,
        // three file operations arriving together after one bad handshake would
        // each start their own - and this guards a step that tears down a live
        // tunnel, so three at once is the thing it exists to prevent.
        const gates = [deferred<{ ok: boolean }>(), deferred<{ ok: boolean }>()];
        let runs = 0;
        const step = cacheSuccess(() => gates[runs++].promise, ok);

        const first = step();
        gates[0].resolve({ ok: false });
        await expect(first).resolves.toEqual({ ok: false });

        const second = step();          // starts attempt 2, still in flight
        const third = step();           // must JOIN it, not start a third
        expect(runs).toBe(2);
        expect(third).toBe(second);

        gates[1].resolve({ ok: true });
        await second;
        expect(runs).toBe(2);
    });
});
