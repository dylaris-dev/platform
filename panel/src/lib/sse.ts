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

/**
 * Subscribe to a stream for as long as the caller wants it, and return the
 * function that ends it.
 *
 * Core ends every stream after a few minutes, so that the reconnect is
 * authorized again: a stream used to be checked once when it opened. The
 * browser reconnects on its own and sends the last event id, which is what
 * lets the console resume without a gap - so an ordinary end is left to it.
 * Only a stream the browser has given up on (CLOSED: the ticket was refused,
 * say after a password change) is opened again, with a fresh ticket.
 */
export function subscribeEventSource(
    path: string,
    onMessage: (e: MessageEvent) => void,
): () => void {
    let es: EventSource | null = null;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let delay = RETRY_START_MS;

    const retry = () => {
        if (stopped || timer) return;
        timer = setTimeout(() => {
            timer = null;
            open();
        }, delay);
        delay = Math.min(delay * 2, RETRY_MAX_MS);
    };

    const open = () => {
        createEventSource(path).then((next) => {
            if (stopped) {
                next.close();
                return;
            }
            es = next;
            next.onmessage = (e) => {
                delay = RETRY_START_MS;
                onMessage(e);
            };
            next.onerror = () => {
                if (next.readyState !== EventSource.CLOSED) return; // the browser is reconnecting
                next.close();
                if (es === next) es = null;
                retry();
            };
        }).catch(retry);
    };

    open();
    return () => {
        stopped = true;
        if (timer) clearTimeout(timer);
        es?.close();
        es = null;
    };
}
