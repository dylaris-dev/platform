// One shared, visibility-gated poller per server for its domains. The header
// address (ServerShell) and the Domains card both subscribe; the poller runs
// while at least one subscriber exists and the tab is visible. A snapshot never
// outlives the truth for long: a 404 clears it, a failed poll marks it stale,
// and the header only trusts a green domain from a recent snapshot.

import { useCallback, useSyncExternalStore } from 'react';
import { getServerDomains, type ServerDomain, type ServerDomains } from '@/lib/api/serverDomains';

export const POLL_MS = 15_000;
// Three missed polls. Older than this, a green domain is no longer evidence.
export const MAX_AGE_MS = 45_000;
// Floor for how old Core's own observation of a domain may be. A fresh poll can
// still carry a state the reconciler wrote long ago (it stopped, or lost the
// leader), and that state says nothing about DNS now.
export const MIN_STATE_AGE_MS = 90_000;

// Two reconcile intervals, at least 90 s. A Core that does not send the
// interval gets the 90 s floor.
export function maxStateAgeMs(intervalSeconds?: number): number {
    return Math.max(2 * (intervalSeconds ?? 0) * 1000, MIN_STATE_AGE_MS);
}

export interface DomainsState {
    data: ServerDomains | null;
    fetchedAt: number;
    // The last poll failed; data (if any) is from an earlier one.
    stale: boolean;
    // Core answered 404: feature off or gateway routing.
    hidden: boolean;
    // Message of the last failed poll, '' otherwise.
    error: string;
}

const EMPTY: DomainsState = { data: null, fetchedAt: 0, stale: false, hidden: false, error: '' };

// Domains the header may promote to the primary address: from a recent
// snapshot, and each observed by Core recently.
export function trustedDomains(s: DomainsState, now: number): ServerDomain[] {
    if (!s.data || s.hidden || now - s.fetchedAt > MAX_AGE_MS) return [];
    const maxAge = maxStateAgeMs(s.data.intervalSeconds);
    return s.data.domains.filter(d => {
        const at = d.observedAt ? Date.parse(d.observedAt) : NaN;
        return Number.isFinite(at) && now - at <= maxAge;
    });
}

export function applyResult(prev: DomainsState, res: ServerDomains, now: number): DomainsState {
    if (res.success) return { data: res, fetchedAt: now, stale: false, hidden: false, error: '' };
    if (res.status === 404) return { ...EMPTY, hidden: true, fetchedAt: now };
    return { ...prev, stale: true, hidden: false, error: res.message || 'Could not load the domains.' };
}

interface Entry {
    state: DomainsState;
    count: number;
    timer: ReturnType<typeof setInterval> | null;
    subs: Set<() => void>;
    onVisible: () => void;
}

const entries = new Map<string, Entry>();

async function poll(id: string) {
    const e = entries.get(id);
    if (!e) return;
    const res = await getServerDomains(id);
    // Unsubscribed while the request was in flight.
    if (entries.get(id) !== e) return;
    e.state = applyResult(e.state, res, Date.now());
    e.subs.forEach(f => f());
}

function run(id: string, e: Entry) {
    if (e.timer) { clearInterval(e.timer); e.timer = null; }
    if (typeof document === 'undefined' || document.visibilityState !== 'visible') return;
    void poll(id);
    e.timer = setInterval(() => void poll(id), POLL_MS);
}

export function subscribeServerDomains(id: string, cb: () => void): () => void {
    let entry = entries.get(id);
    if (!entry) {
        const created: Entry = { state: EMPTY, count: 0, timer: null, subs: new Set(), onVisible: () => run(id, created) };
        entry = created;
        entries.set(id, entry);
    }
    const e = entry;
    e.subs.add(cb);
    if (++e.count === 1) {
        document.addEventListener('visibilitychange', e.onVisible);
        run(id, e);
    }
    return () => {
        e.subs.delete(cb);
        if (--e.count === 0) {
            if (e.timer) clearInterval(e.timer);
            document.removeEventListener('visibilitychange', e.onVisible);
            entries.delete(id);
        }
    };
}

export const getServerDomainsState = (id: string): DomainsState => entries.get(id)?.state ?? EMPTY;

// Subscribes for as long as the component is mounted. `enabled` false = no poller.
export function useServerDomains(id: string, enabled = true): DomainsState & { refresh: () => Promise<void> } {
    const sub = useCallback((cb: () => void) => (enabled ? subscribeServerDomains(id, cb) : () => {}), [id, enabled]);
    const state = useSyncExternalStore(sub, () => (enabled ? getServerDomainsState(id) : EMPTY), () => EMPTY);
    const refresh = useCallback(() => poll(id), [id]);
    return { ...state, refresh };
}
