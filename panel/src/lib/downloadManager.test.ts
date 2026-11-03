import { describe, it, expect, vi } from 'vitest';
import { createDownloadStore, downloadPercent, downloadSpeed, downloadEta, aggregatePercent } from './downloadManager';

function clockStore() {
    let t = 1_000_000;
    const notify = vi.fn();
    const store = createDownloadStore({ now: () => t, notify });
    return { store, notify, advance: (ms: number) => { t += ms; }, at: () => t };
}

// A run the test finishes by hand, with the progress callback it was given.
function deferredRun() {
    let resolve!: () => void;
    let reject!: (e: unknown) => void;
    let report!: (loaded: number, total: number, name?: string, path?: string, note?: string) => void;
    const run = (onProgress: typeof report) => {
        report = onProgress;
        return new Promise<void>((res, rej) => { resolve = res; reject = rej; });
    };
    return { run, resolve: () => resolve(), reject: (e: unknown) => reject(e), report: (...a: Parameters<typeof report>) => report(...a) };
}

describe('download store', () => {
    it('holds the percent at 99 until the download resolves, then shows 100', async () => {
        const { store } = clockStore();
        const d = deferredRun();
        const done = store.track('world.zip', 0, d.run);
        d.report(0, 1000, 'world.zip', '/home/u/world.zip');
        d.report(1050, 1000); // the zip came out larger than the estimate
        const e = store.getSnapshot()[0];
        expect(e.total).toBe(1000);
        expect(downloadPercent(e)).toBe(99);
        expect(downloadEta(e, Date.now())).toBeNull();
        d.resolve();
        expect(await done).toBe(true);
        const fin = store.getSnapshot()[0];
        expect(fin.state).toBe('done');
        expect(downloadPercent(fin)).toBe(100);
        expect(fin.savePath).toBe('/home/u/world.zip');
    });

    it('reports no percent while the total is unknown', () => {
        const { store } = clockStore();
        const id = store.start('a.zip', 0);
        store.progress(id, 500, 0);
        expect(downloadPercent(store.getSnapshot()[0])).toBeNull();
        expect(aggregatePercent(store.getSnapshot())).toBeNull();
    });

    it('keeps the starting total when an event carries none', () => {
        const { store } = clockStore();
        const id = store.start('big.bin', 4000);
        store.progress(id, 1000, 0);
        expect(store.getSnapshot()[0].total).toBe(4000);
        expect(downloadPercent(store.getSnapshot()[0])).toBe(25);
    });

    it('smooths the speed instead of jumping to each sample', () => {
        const { store, advance, at } = clockStore();
        const id = store.start('f', 100_000_000);
        store.progress(id, 0, 100_000_000); // save dialog closed, nothing yet
        advance(10_000); // the wait before the first byte is not averaged in
        store.progress(id, 0, 100_000_000);
        advance(1000);
        store.progress(id, 1_000_000, 100_000_000);
        expect(store.getSnapshot()[0].speed).toBeCloseTo(1_000_000, -3);
        advance(1000);
        store.progress(id, 4_000_000, 100_000_000); // one fast second: 3 MB/s
        const s = store.getSnapshot()[0].speed;
        expect(s).toBeGreaterThan(1_000_000);
        expect(s).toBeLessThan(3_000_000);
        // Samples closer than the minimum are folded into the next one.
        advance(10);
        store.progress(id, 4_500_000, 100_000_000);
        expect(store.getSnapshot()[0].speed).toBe(s);
        const e = store.getSnapshot()[0];
        expect(downloadEta(e, at())).toBe(Math.ceil((100_000_000 - 4_500_000) / s));
        // A stalled transfer shows no speed rather than a stale one.
        expect(downloadSpeed(e, at() + 6000)).toBe(0);
    });

    it('records a failure with its message and tells the user', async () => {
        const { store, notify } = clockStore();
        const ok = await store.track('x.jar', 10, async (p) => { p(5, 10); throw new Error('relay closed the stream'); });
        expect(ok).toBe(false);
        const e = store.getSnapshot()[0];
        expect(e.state).toBe('failed');
        expect(e.error).toBe('relay closed the stream');
        expect(notify).toHaveBeenCalledWith('Download of x.jar failed: relay closed the stream', false);
    });

    it('drops a download whose save dialog was cancelled', async () => {
        const { store, notify } = clockStore();
        expect(await store.track('x.jar', 10, async () => {})).toBe(true);
        expect(store.getSnapshot()).toEqual([]);
        expect(notify).not.toHaveBeenCalled();
    });

    it('runs several downloads as separate entries, and clears only finished ones', async () => {
        const { store } = clockStore();
        const a = deferredRun();
        const b = deferredRun();
        const pa = store.track('a', 100, a.run);
        const pb = store.track('b', 200, b.run);
        a.report(50, 100);
        b.report(20, 200);
        expect(store.getSnapshot().map(e => [e.filename, e.loaded])).toEqual([['a', 50], ['b', 20]]);
        expect(aggregatePercent(store.getSnapshot())).toBe(23); // 70 of 300
        a.resolve();
        await pa;
        expect(store.lastDoneId()).toBe(store.getSnapshot()[0].id);
        store.clearFinished();
        expect(store.getSnapshot().map(e => e.filename)).toEqual(['b']);
        expect(store.lastDoneId()).toBeNull();
        b.resolve();
        await pb;
        expect(store.getSnapshot()[0].state).toBe('done');
    });

    it('ignores progress after an entry finished', async () => {
        const { store } = clockStore();
        const d = deferredRun();
        const p = store.track('a', 100, d.run);
        d.report(100, 100);
        d.resolve();
        await p;
        d.report(5, 100);
        expect(store.getSnapshot()[0].loaded).toBe(100);
    });

    it('cancels through the app by save path and ends as cancelled, not failed', async () => {
        const cancel = vi.fn();
        const notify = vi.fn();
        const store = createDownloadStore({ notify, cancel });
        const d = deferredRun();
        const p = store.track('world.zip', 0, d.run);
        // Not cancellable before the app said where the file goes.
        expect(store.canCancel(store.getSnapshot()[0])).toBe(false);
        d.report(10, 100, 'world.zip', 'C:/dl/world.zip');
        const id = store.getSnapshot()[0].id;
        store.cancel(id);
        expect(cancel).toHaveBeenCalledWith('C:/dl/world.zip');
        expect(store.canCancel(store.getSnapshot()[0])).toBe(false);
        store.cancel(id);
        expect(cancel).toHaveBeenCalledTimes(1);
        d.reject(new Error('cancelled'));
        expect(await p).toBe(false);
        expect(store.getSnapshot()[0].state).toBe('cancelled');
        expect(notify).not.toHaveBeenCalled();
    });

    it('cannot cancel without an app that can', () => {
        const store = createDownloadStore();
        const d = deferredRun();
        void store.track('a', 0, d.run);
        d.report(1, 10, 'a', '/a');
        expect(store.canCancel(store.getSnapshot()[0])).toBe(false);
    });

    it('shows the retry note and restarts the speed when the app starts over', () => {
        const { store, advance } = clockStore();
        const d = deferredRun();
        void store.track('w.zip', 1000, d.run);
        d.report(0, 1000, 'w.zip', '/w.zip');
        advance(1000);
        d.report(500, 1000);
        expect(store.getSnapshot()[0].speed).toBeGreaterThan(0);
        advance(1000);
        d.report(0, 0, 'w.zip', '/w.zip', 'Connection lost, retrying (attempt 2)');
        const e = store.getSnapshot()[0];
        expect(e.note).toBe('Connection lost, retrying (attempt 2)');
        expect(e.loaded).toBe(0);
        expect(e.total).toBe(1000);
        expect(e.speed).toBe(0);
        advance(1000);
        d.report(300, 1000);
        const after = store.getSnapshot()[0];
        expect(after.note).toBeUndefined();
        expect(after.speed).toBeGreaterThan(0);
    });
});
