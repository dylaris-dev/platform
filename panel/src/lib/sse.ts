// Shared EventSource factory that keeps the long-lived session JWT out of
// URLs. EventSource cannot set an Authorization header, so the only way to
// authenticate an SSE stream is via the URL — and putting the session JWT
// there leaks it into access logs / the Referer header.
//
// Instead we mint a short-lived, single-purpose ticket (POST /api/sse-ticket,
// authenticated with the normal Bearer header) and carry that in ?ticket=.
// The ticket expires in 5 minutes server-side with a sliding window, so
// native EventSource auto-reconnects within the window reuse the same URL. It
// opens the streams and nothing else, and a password change ends it.

import { API_URL, getAuthHeader } from '@/lib/api/core';
import { handleUnauthorized } from '@/lib/api/session';

/**
 * Mint an SSE ticket and open an EventSource for `path` (e.g.
 * "/system/events" or "/servers/1/console/stream?sub_server=foo").
 * Throws if the ticket mint fails so the caller's existing error path runs.
 */
export async function createEventSource(path: string): Promise<EventSource> {
    const res = await fetch(`${API_URL}/sse-ticket`, {
        method: 'POST',
        headers: getAuthHeader(),
    });
    if (!res.ok) {
        // A stream cannot report a dead session itself, so the ticket mint is
        // where it shows: without this, every live view retried forever and the
        // page sat on "reconnecting" until someone reloaded it.
        handleUnauthorized(res);
        throw new Error(`sse-ticket mint failed: ${res.status}`);
    }
    const data = await res.json();
    const ticket: string = data.ticket;
    if (!ticket) {
        throw new Error('sse-ticket mint returned no ticket');
    }
    const sep = path.includes('?') ? '&' : '?';
    return new EventSource(`${API_URL}${path}${sep}ticket=${encodeURIComponent(ticket)}`);
}

const RETRY_START_MS = 1000;
const RETRY_MAX_MS = 30000;
// Core ends every stream after a few minutes and the browser reopens it at
// once. A drop that is open again within this window is not worth a badge.
export const RECONNECT_GRACE_MS = 3000;

// Live: open. Reconnecting: the browser, or a retry soon, is reopening it.
// Disconnected: retries keep failing; the next one may be up to 30 s away.
export type StreamStatus = 'connecting' | 'live' | 'reconnecting' | 'disconnected';

export interface SubscribeOptions {
    // Query parameter that names the last event id when a stream the browser
    // gave up on is opened again. A new EventSource starts without
    // Last-Event-ID, so without it everything sent while it was down is lost.
    resumeParam?: string;
    onStatus?: (status: StreamStatus) => void;
}

// The stop function, which also offers reconnecting at once.
export type Subscription = (() => void) & { reconnect: () => void };

// withResumeParam names the resume position on a stream path.
export function withResumeParam(path: string, param: string | undefined, lastId: string): string {
    if (!param || !lastId) return path;
    const sep = path.includes('?') ? '&' : '?';
    return `${path}${sep}${param}=${encodeURIComponent(lastId)}`;
}

/**
 * Subscribe to a stream for as long as the caller wants it, and return the
 * function that ends it.
 *
 * Core ends every stream after a few minutes, so that the reconnect is
 * authorized again: a stream used to be checked once when it opened. The
 * browser reconnects on its own and sends the last event id, which is what
 * lets the console resume without a gap - so an ordinary end is left to it.
 * Only a stream the browser has given up on (CLOSED: the ticket was refused,
 * say after a password change) is opened again, with a fresh ticket, and with
 * opts.resumeParam naming the last event id seen.
 */
export function subscribeEventSource(
    path: string,
    onMessage: (e: MessageEvent) => void,
    // Named events (`event: <name>`), which onmessage never sees.
    events?: Record<string, (e: MessageEvent) => void>,
    opts: SubscribeOptions = {},
): Subscription {
    let es: EventSource | null = null;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = RETRY_START_MS;
    let lastId = '';
    // Bumped per open, so a ticket minted for a superseded attempt is dropped.
    let attempt = 0;
    let status: StreamStatus | null = null;
    let grace: ReturnType<typeof setTimeout> | null = null;
    const setStatus = (s: StreamStatus) => {
        if (grace) { clearTimeout(grace); grace = null; }
        if (stopped || s === status) return;
        status = s;
        opts.onStatus?.(s);
    };
    // A live stream that drops is reported only if it is still down after the
    // grace window; any status set meanwhile (live again, above all) wins.
    const degrade = (s: StreamStatus) => {
        if (status !== 'live') { setStatus(s); return; }
        if (grace) clearTimeout(grace);
        grace = setTimeout(() => { grace = null; setStatus(s); }, RECONNECT_GRACE_MS);
    };

    const retry = () => {
        if (stopped || timer) return;
        degrade(delay > RETRY_START_MS * 2 ? 'disconnected' : 'reconnecting');
        timer = setTimeout(() => {
            timer = null;
            open();
        }, delay);
        delay = Math.min(delay * 2, RETRY_MAX_MS);
    };

    const open = () => {
        const mine = ++attempt;
        createEventSource(withResumeParam(path, opts.resumeParam, lastId)).then((next) => {
            if (stopped || mine !== attempt) {
                next.close();
                return;
            }
            es = next;
            // Open is the success the backoff waits for. A quiet console sends
            // no message for minutes, so resetting only on a message let every
            // rollover push the next delay further out.
            next.onopen = () => {
                delay = RETRY_START_MS;
                setStatus('live');
            };
            next.onmessage = (e) => {
                delay = RETRY_START_MS;
                if (e.lastEventId) lastId = e.lastEventId;
                setStatus('live');
                onMessage(e);
            };
            for (const [name, handler] of Object.entries(events ?? {})) {
                next.addEventListener(name, handler as EventListener);
            }
            next.onerror = () => {
                if (next.readyState !== EventSource.CLOSED) {
                    degrade('reconnecting'); // the browser is reconnecting
                    return;
                }
                next.close();
                if (es === next) es = null;
                retry();
            };
        }).catch(() => {
            if (mine === attempt) retry();
        });
    };

    setStatus('connecting');
    open();
    const stop = () => {
        stopped = true;
        if (timer) clearTimeout(timer);
        if (grace) clearTimeout(grace);
        es?.close();
        es = null;
    };
    return Object.assign(stop, {
        reconnect: () => {
            if (stopped) return;
            if (timer) clearTimeout(timer);
            timer = null;
            delay = RETRY_START_MS;
            es?.close();
            es = null;
            setStatus('connecting');
            open();
        },
    });
}
