// panel-side client for the /api/system/events SSE stream.
//
// One EventSource per session, opened by AppDataContext on boot. Callers
// subscribe via `systemEvents.on(type, cb)` and get an unsubscribe closure.
// Reconnects with exponential backoff (1s → 30s cap) so a Core restart or
// brief network blip recovers without panel reload.
//
// The wire envelope is { type: "regions.changed"|..., payload?: {} }. Payload
// is usually omitted — listeners just re-fetch their cached list when their
// event fires.

import { createEventSource } from '@/lib/sse';

export interface SystemEvent {
    type: string;
    payload?: Record<string, unknown>;
}

type Listener = (event: SystemEvent) => void;
type Subscription = { type: string | '*'; cb: Listener };

const INITIAL_RECONNECT_MS = 1000;
const MAX_RECONNECT_MS = 30_000;

class SystemEventsClient {
    private es: EventSource | null = null;
    private subs = new Set<Subscription>();
    private reconnectDelay = INITIAL_RECONNECT_MS;
    private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    private stopped = true;

    /** Open the stream. Idempotent — calling twice is a no-op. */
    start(): void {
        if (!this.stopped) return;
        this.stopped = false;
        this.connect();
    }

    /** Close the stream and cancel any pending reconnect. */
    stop(): void {
        this.stopped = true;
        if (this.reconnectTimer) {
            clearTimeout(this.reconnectTimer);
            this.reconnectTimer = null;
        }
        if (this.es) {
            this.es.close();
            this.es = null;
        }
    }

    /**
     * Subscribe to one event type or all ('*'). Returns an unsubscribe fn.
     * Listener errors are swallowed so one broken consumer can't kill others.
     */
    on(type: string | '*', cb: Listener): () => void {
        const sub: Subscription = { type, cb };
        this.subs.add(sub);
        return () => { this.subs.delete(sub); };
    }

    private connect(): void {
        if (this.stopped) return;
        // Ticket mint is async (it POSTs for a short-lived SSE ticket). Kick
        // it off and guard against stop()/reconnect racing it: if we got
        // stopped (or a newer connect() already attached an es) before the
        // ticket resolves, close the freshly-opened stream and bail.
        createEventSource('/system/events').then((es) => {
            if (this.stopped || this.es) {
                es.close();
                return;
            }
            this.es = es;

            es.onmessage = (e) => {
                try {
                    const evt: SystemEvent = JSON.parse(e.data);
                    if (evt && typeof evt.type === 'string') {
                        this.dispatch(evt);
                        // First good message resets backoff so a flaky network
                        // doesn't compound delays across multiple reconnects.
                        this.reconnectDelay = INITIAL_RECONNECT_MS;
                    }
                } catch { /* malformed frames are silently dropped */ }
            };

            es.onerror = () => {
                es.close();
                this.es = null;
                this.scheduleReconnect();
            };
        }).catch(() => {
            // Mint failed (no token, network blip, Core down) — behave like a
            // connection failure and let the existing backoff retry fire.
            this.scheduleReconnect();
        });
    }

    private scheduleReconnect(): void {
        if (this.stopped) return;
        if (this.reconnectTimer) return;
        const delay = this.reconnectDelay;
        this.reconnectDelay = Math.min(this.reconnectDelay * 2, MAX_RECONNECT_MS);
        this.reconnectTimer = setTimeout(() => {
            this.reconnectTimer = null;
            this.connect();
        }, delay);
    }

    private dispatch(evt: SystemEvent): void {
        // Snapshot the set first so a listener that calls .on/.off during
        // dispatch can't mutate-while-iterating.
        for (const sub of Array.from(this.subs)) {
            if (sub.type === '*' || sub.type === evt.type) {
                try { sub.cb(evt); } catch { /* ignore */ }
            }
        }
    }
}

export const systemEvents = new SystemEventsClient();

// A window event, not a server one: the server header keeps its own route list,
// and the Setup tab in the same page tells it about a route it just created.
export const ROUTES_CHANGED_EVENT = 'dylaris:routes.changed';
