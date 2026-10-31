import { describe, it, expect, vi, afterEach } from 'vitest';
import { withNativeProgress } from './wailsBeamAdapter';

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
});
