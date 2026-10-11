import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { subscribeEventSource, withResumeParam, RECONNECT_GRACE_MS, type StreamStatus } from './sse';
import { appendLive } from './consoleLines';

class FakeEventSource {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 2;
    static all: FakeEventSource[] = [];
    readyState = FakeEventSource.CONNECTING;
    onopen: (() => void) | null = null;
    onmessage: ((e: MessageEvent) => void) | null = null;
    onerror: (() => void) | null = null;
    constructor(public url: string) {
        FakeEventSource.all.push(this);
    }
    addEventListener() {}
    close() {
        this.readyState = FakeEventSource.CLOSED;
    }
    emit(id: string, data: string) {
        this.onmessage?.({ lastEventId: id, data } as MessageEvent);
    }
    // The browser gave up (a refused ticket): EventSource goes CLOSED.
    die() {
        this.readyState = FakeEventSource.CLOSED;
        this.onerror?.();
    }
}

const flush = () => new Promise(r => setTimeout(r, 0));

describe('withResumeParam', () => {
    it.each([
        ['/s/1/console/stream', 'after', '', '/s/1/console/stream'],
        ['/s/1/console/stream', undefined, '5-0', '/s/1/console/stream'],
        ['/s/1/console/stream', 'after', '5-0', '/s/1/console/stream?after=5-0'],
        ['/s/1/console/stream?sub_server=a', 'after', '5-0', '/s/1/console/stream?sub_server=a&after=5-0'],
    ])('%s %s %s', (path, param, id, want) => {
        expect(withResumeParam(path, param, id)).toBe(want);
    });
});

describe('subscribeEventSource resume', () => {
    beforeEach(() => {
        FakeEventSource.all = [];
        vi.stubGlobal('EventSource', FakeEventSource);
        vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, json: async () => ({ ticket: 't' }) })));
    });
    afterEach(() => {
        vi.unstubAllGlobals();
        vi.useRealTimers();
    });

    it('reopens a dead stream after the last event it saw', async () => {
        const statuses: StreamStatus[] = [];
        const sub = subscribeEventSource('/servers/1/console/stream', () => {}, undefined, {
            resumeParam: 'after',
            onStatus: s => statuses.push(s),
        });
        await flush();
        const first = FakeEventSource.all[0];
        expect(first.url).not.toContain('after=');
        first.onopen?.();
        first.emit('7-0', 'a');
        first.emit('8-0', 'b');
        first.die();
        sub.reconnect();
        await flush();
        const second = FakeEventSource.all[1];
        expect(second.url).toContain('/servers/1/console/stream?after=8-0&ticket=');
        // The drop is inside the grace window when reconnect() supersedes it.
        expect(statuses).toEqual(['connecting', 'live', 'connecting']);
        sub();
    });

    it('reports disconnected once retries keep failing', async () => {
        vi.useFakeTimers();
        const statuses: StreamStatus[] = [];
        vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 401 })));
        const sub = subscribeEventSource('/x', () => {}, undefined, { onStatus: s => statuses.push(s) });
        await vi.advanceTimersByTimeAsync(1000 + 2000 + 10);
        expect(statuses).toEqual(['connecting', 'reconnecting', 'disconnected']);
        sub();
    });

    // Core ends every console stream after 5 minutes and the browser reopens it
    // at once; each rollover used to flash Reconnecting.
    it('a rollover the browser reopens within the grace window shows nothing', async () => {
        vi.useFakeTimers();
        const statuses: StreamStatus[] = [];
        const sub = subscribeEventSource('/x', () => {}, undefined, { onStatus: s => statuses.push(s) });
        await vi.advanceTimersByTimeAsync(0);
        const es = FakeEventSource.all[0];
        es.onopen?.();
        es.onerror?.(); // readyState CONNECTING: the browser is reopening it
        await vi.advanceTimersByTimeAsync(RECONNECT_GRACE_MS - 500);
        es.onopen?.();
        await vi.advanceTimersByTimeAsync(10_000);
        expect(statuses).toEqual(['connecting', 'live']);
        sub();
    });

    it('a drop still down after the grace window is reported', async () => {
        vi.useFakeTimers();
        const statuses: StreamStatus[] = [];
        const sub = subscribeEventSource('/x', () => {}, undefined, { onStatus: s => statuses.push(s) });
        await vi.advanceTimersByTimeAsync(0);
        const es = FakeEventSource.all[0];
        es.onopen?.();
        es.onerror?.();
        await vi.advanceTimersByTimeAsync(RECONNECT_GRACE_MS - 10);
        expect(statuses).toEqual(['connecting', 'live']);
        await vi.advanceTimersByTimeAsync(20);
        expect(statuses).toEqual(['connecting', 'live', 'reconnecting']);
        sub();
    });

    // A quiet console sends no message, so a backoff reset only on messages
    // grew with every rollover until a routine one read as Disconnected.
    it('opening resets the backoff, so quiet rollovers never read as disconnected', async () => {
        vi.useFakeTimers();
        const statuses: StreamStatus[] = [];
        const sub = subscribeEventSource('/x', () => {}, undefined, { onStatus: s => statuses.push(s) });
        await vi.advanceTimersByTimeAsync(0);
        for (let i = 0; i < 4; i++) {
            const es = FakeEventSource.all.at(-1)!;
            es.onopen?.();
            es.die();
            await vi.advanceTimersByTimeAsync(RECONNECT_GRACE_MS + 10);
        }
        expect(statuses).not.toContain('disconnected');
        sub();
    });

    it('a stream without resumeParam reopens at its own path', async () => {
        const sub = subscribeEventSource('/servers/1/stats/stream', () => {});
        await flush();
        FakeEventSource.all[0].emit('3-0', 'x');
        sub.reconnect();
        await flush();
        expect(FakeEventSource.all[1].url).not.toContain('after=');
        sub();
    });
});

describe('replayed lines', () => {
    it('are not shown twice', () => {
        let lines = appendLive([], { key: '7-0', text: 'a' }, true);
        lines = appendLive(lines, { key: '8-0', text: 'b' }, true);
        // A resume after 7-0 replays 8-0 again before the new line.
        lines = appendLive(lines, { key: '8-0', text: 'b' }, true);
        lines = appendLive(lines, { key: '9-0', text: 'c' }, true);
        expect(lines.map(l => l.key)).toEqual(['7-0', '8-0', '9-0']);
    });
});
