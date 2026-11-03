import type { DownloadProgressFn, DownloadTracker } from '@dylaris/ui-filebrowser';

// The panel's global download list, behind the header download button.
//
// A module-level store rather than state in the file browser, so a download
// keeps its entry when the user leaves the page that started it, and several
// can run at once. The file browser reaches it through DownloadTrackerContext
// (mounted in the authed layout); without that provider it keeps its own card.

export type DownloadState = 'running' | 'done' | 'failed';

export interface DownloadEntry {
    id: number;
    filename: string;
    loaded: number;
    // 0 = unknown. A folder zip's total is an estimate (sum of the file sizes),
    // so loaded can pass it, and the percent is held at 99 until the end.
    total: number;
    state: DownloadState;
    error?: string;
    // Where a native save dialog put the file. A browser download has none.
    savePath?: string;
    startedAt: number;
    finishedAt?: number;
    // Smoothed bytes/second, and the sample it was last computed from.
    speed: number;
    sampleAt: number;
    sampleLoaded: number;
    lastEventAt: number;
    // Whether a single progress report arrived. A native download whose save
    // dialog was cancelled resolves without one.
    reported: boolean;
}

// Time constant of the speed average: long enough that the number can be
// read, short enough that a real change shows within a few seconds.
const SPEED_TAU_MS = 3000;
// Samples closer together than this are folded into the next one, so a burst
// of small browser chunks does not read as a spike.
const SPEED_MIN_SAMPLE_MS = 250;
// No byte for this long, and the shown speed is 0 rather than a stale number.
const STALL_MS = 5000;
// Progress reaches subscribers at most this often; state changes immediately.
const NOTIFY_MS = 150;

export interface DownloadStoreOptions {
    now?: () => number;
    // Short message on completion or failure (the panel's toast).
    notify?: (message: string, ok: boolean) => void;
}

export function createDownloadStore(opts: DownloadStoreOptions = {}) {
    const now = opts.now ?? Date.now;
    let entries: DownloadEntry[] = [];
    let nextId = 1;
    // The entry that finished last: the only one the app can show in its
    // folder, because it remembers just the last saved path.
    let lastDoneId: number | null = null;
    const listeners = new Set<() => void>();
    let timer: ReturnType<typeof setTimeout> | null = null;

    const emit = () => {
        if (timer) { clearTimeout(timer); timer = null; }
        listeners.forEach(l => l());
    };
    const emitSoon = () => {
        if (!timer) timer = setTimeout(emit, NOTIFY_MS);
    };
    const patch = (id: number, fn: (e: DownloadEntry) => DownloadEntry) => {
        entries = entries.map(e => (e.id === id ? fn(e) : e));
    };

    const start = (filename: string, total: number): number => {
        const t = now();
        const id = nextId++;
        entries = [...entries, {
            id, filename, loaded: 0, total: total > 0 ? total : 0, state: 'running',
            startedAt: t, speed: 0, sampleAt: t, sampleLoaded: 0, lastEventAt: t, reported: false,
        }];
        emit();
        return id;
    };

    const progress = (id: number, loaded: number, total: number, filename?: string, savePath?: string) => {
        const t = now();
        patch(id, e => {
            if (e.state !== 'running') return e;
            const next: DownloadEntry = {
                ...e,
                loaded,
                // An event without a total keeps the one known at the start.
                total: total > 0 ? total : e.total,
                filename: filename || e.filename,
                savePath: savePath || e.savePath,
                lastEventAt: t,
                reported: true,
            };
            const dt = t - e.sampleAt;
            if (e.sampleLoaded === 0 && loaded === 0) {
                // Nothing moved yet (a save dialog, a zip being built): the
                // first real sample must not average that wait in.
                next.sampleAt = t;
            } else if (dt >= SPEED_MIN_SAMPLE_MS) {
                const inst = Math.max(0, ((loaded - e.sampleLoaded) * 1000) / dt);
                const alpha = 1 - Math.exp(-dt / SPEED_TAU_MS);
                next.speed = e.speed === 0 && e.sampleLoaded === 0 ? inst : e.speed + alpha * (inst - e.speed);
                next.sampleAt = t;
                next.sampleLoaded = loaded;
            }
            return next;
        });
        emitSoon();
    };

    const finish = (id: number) => {
        const e = entries.find(x => x.id === id);
        if (!e || e.state !== 'running') return;
        if (!e.reported) {
            // Resolved without a byte reported: the save dialog was cancelled.
            remove(id);
            return;
        }
        patch(id, x => ({ ...x, state: 'done', finishedAt: now(), loaded: Math.max(x.loaded, x.total), total: Math.max(x.loaded, x.total) }));
        lastDoneId = id;
        emit();
        opts.notify?.(`Downloaded ${e.filename}.`, true);
    };

    const fail = (id: number, message: string) => {
        const e = entries.find(x => x.id === id);
        if (!e || e.state !== 'running') return;
        patch(id, x => ({ ...x, state: 'failed', error: message, finishedAt: now() }));
        emit();
        opts.notify?.(`Download of ${e.filename} failed: ${message}`, false);
    };

    const remove = (id: number) => {
        entries = entries.filter(e => e.id !== id);
        if (lastDoneId === id) lastDoneId = null;
        emit();
    };

    const clearFinished = () => {
        entries = entries.filter(e => e.state === 'running');
        if (lastDoneId !== null && !entries.some(e => e.id === lastDoneId)) lastDoneId = null;
        emit();
    };

    const track: DownloadTracker['track'] = async (filename, total, run) => {
        const id = start(filename, total);
        const onProgress: DownloadProgressFn = (loaded, t, name, savePath) => progress(id, loaded, t, name, savePath);
        try {
            await run(onProgress);
            finish(id);
            return true;
        } catch (err) {
            fail(id, errorMessage(err));
            return false;
        }
    };

    return {
        subscribe(l: () => void) {
            listeners.add(l);
            return () => { listeners.delete(l); };
        },
        getSnapshot: () => entries,
        lastDoneId: () => lastDoneId,
        start, progress, finish, fail, remove, clearFinished, track,
    };
}

export type DownloadStore = ReturnType<typeof createDownloadStore>;

function errorMessage(err: unknown): string {
    const msg = err instanceof Error ? err.message : typeof err === 'string' ? err : '';
    return msg.trim() || 'Unknown error';
}

// Percent to show, or null when the total is unknown. Held at 99 while the
// download runs: a zip's total is an estimate, and only the finished download
// is 100.
export function downloadPercent(e: DownloadEntry): number | null {
    if (e.state === 'done') return 100;
    if (e.total <= 0) return null;
    return Math.min(99, Math.floor((e.loaded / e.total) * 100));
}

export function downloadSpeed(e: DownloadEntry, at: number): number {
    if (e.state !== 'running' || at - e.lastEventAt > STALL_MS) return 0;
    return e.speed;
}

// Seconds left, or null when it cannot be told: no total, no speed, or past
// an estimated total.
export function downloadEta(e: DownloadEntry, at: number): number | null {
    const speed = downloadSpeed(e, at);
    if (e.total <= 0 || speed <= 0 || e.loaded >= e.total) return null;
    return Math.ceil((e.total - e.loaded) / speed);
}

// Overall percent across the running downloads that know their total, or
// null when none does.
export function aggregatePercent(entries: DownloadEntry[]): number | null {
    const known = entries.filter(e => e.state === 'running' && e.total > 0);
    if (known.length === 0) return null;
    const loaded = known.reduce((s, e) => s + Math.min(e.loaded, e.total), 0);
    const total = known.reduce((s, e) => s + e.total, 0);
    return Math.min(99, Math.floor((loaded / total) * 100));
}
