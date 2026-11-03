import { describe, it, expect, vi, afterEach } from 'vitest';
import { withNativeProgress, routeNativeProgress } from './wailsBeamAdapter';

// A native Beam download resolves only when the file is complete, and the
// app reports the bytes in between as "download:progress" events. Nothing
// listened, so the file browser showed 0 B for the whole of a 36 GB world
// while it was arriving at 34 MB/s.

type Handler = (data: unknown) => void;

function fakeRuntime() {
    const handlers = new Map<string, Handler>();
    const off = vi.fn();
    vi.stubGlobal('window', {
        runtime: {
            EventsOn: (name: string, cb: Handler) => { handlers.set(name, cb); return off; },
        },
    });
    return { emit: (data: unknown) => handlers.get('download:progress')?.(data), off };
}

afterEach(() => { vi.unstubAllGlobals(); });

describe('withNativeProgress', () => {
    it('forwards the app events, with the name picked in the save dialog', async () => {
        const rt = fakeRuntime();
        const seen: [number, number, string | undefined][] = [];
        await withNativeProgress((l, t, n) => seen.push([l, t, n]), async () => {
            rt.emit({ loaded: 0, total: 0, name: 'download.zip' });
            rt.emit({ loaded: 5_607_179_833, total: 0, name: 'download.zip' });
        });
        expect(seen).toEqual([[0, 0, 'download.zip'], [5_607_179_833, 0, 'download.zip']]);
        expect(rt.off).toHaveBeenCalledTimes(1);
    });

    it('leaves the name out when an older app does not send one', async () => {
        const rt = fakeRuntime();
        const seen: (string | undefined)[] = [];
        await withNativeProgress((_l, _t, n) => seen.push(n), async () => { rt.emit({ loaded: 1, total: 2 }); });
        expect(seen).toEqual([undefined]);
    });

    it('stops listening when the download fails', async () => {
        const rt = fakeRuntime();
        await expect(withNativeProgress(() => {}, async () => { throw new Error('stalled'); })).rejects.toThrow('stalled');
        expect(rt.off).toHaveBeenCalledTimes(1);
    });

    it('still runs the download without an event bridge', async () => {
        vi.stubGlobal('window', {});
        const run = vi.fn(async () => {});
        await withNativeProgress(() => {}, run);
        expect(run).toHaveBeenCalledTimes(1);
    });

    // Two downloads at once: each must see only its own bytes.
    function twoRuns() {
        const gates: (() => void)[] = [];
        const run = () => new Promise<void>(res => { gates.push(res); });
        return { run, finish: (i: number) => gates[i]() };
    }

    it('routes concurrent downloads by their save path', async () => {
        const rt = fakeRuntime();
        const a: [number, string | undefined][] = [];
        const b: [number, string | undefined][] = [];
        const g = twoRuns();
        const pa = withNativeProgress((l, _t, _n, p) => a.push([l, p]), g.run);
        rt.emit({ loaded: 0, total: 10, path: '/d/a.zip' }); // a's dialog closed: a claims it
        const pb = withNativeProgress((l, _t, _n, p) => b.push([l, p]), g.run);
        rt.emit({ loaded: 0, total: 20, path: '/d/b.zip' }); // the next unclaimed path is b's
        rt.emit({ loaded: 5, total: 10, path: '/d/a.zip' });
        rt.emit({ loaded: 7, total: 20, path: '/d/b.zip' });
        g.finish(0);
        await pa;
        rt.emit({ loaded: 10, total: 10, path: '/d/a.zip' }); // late event of a finished download
        rt.emit({ loaded: 9, total: 20, path: '/d/b.zip' });
        g.finish(1);
        await pb;
        expect(a).toEqual([[0, '/d/a.zip'], [5, '/d/a.zip']]);
        expect(b).toEqual([[0, '/d/b.zip'], [7, '/d/b.zip'], [9, '/d/b.zip']]);
        expect(rt.off).toHaveBeenCalledTimes(1); // one shared listener, removed with the last call
    });

    it('sends path-less events of an older app to the newest download', async () => {
        const rt = fakeRuntime();
        const a: number[] = [];
        const b: number[] = [];
        const g = twoRuns();
        const pa = withNativeProgress(l => a.push(l), g.run);
        rt.emit({ loaded: 1, total: 0 });
        const pb = withNativeProgress(l => b.push(l), g.run);
        rt.emit({ loaded: 2, total: 0 });
        g.finish(1);
        await pb;
        rt.emit({ loaded: 3, total: 0 });
        g.finish(0);
        await pa;
        expect(a).toEqual([1, 3]);
        expect(b).toEqual([2]);
    });
});

describe('routeNativeProgress', () => {
    it('does not let a just-finished path claim a waiting download', () => {
        const sink = { onProgress: () => {} };
        expect(routeNativeProgress([sink], '/d/old.zip', new Set(['/d/old.zip']))).toBeUndefined();
        expect(routeNativeProgress([sink], '/d/new.zip', new Set(['/d/old.zip']))).toBe(sink);
        expect(sink).toMatchObject({ path: '/d/new.zip' });
    });

    it('drops an event whose path nobody holds once every call has one', () => {
        const sinks = [{ path: '/d/a', onProgress: () => {} }];
        expect(routeNativeProgress(sinks, '/d/x', new Set())).toBeUndefined();
    });
});
