"use client";

import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import { getInfrastructureOverview, getNodes, getPlayersOnline, GatewayEdge, GatewayLink } from '@/lib/api';
import { flattenServiceErrors, type FlatServiceError } from '@/lib/serviceErrors';
import type { CustomerSummary, InfrastructureData, NodeInfo } from './InfraCards';

/**
 * ONE fetch for the whole infrastructure screen.
 *
 * The screen is now a route per tab, and the obvious shape - each page fetches
 * what it shows - would multiply a single overview request by the number of
 * tabs and make every tab switch a fresh round trip. So the layout fetches and
 * the pages read from here.
 *
 * Everything a page needs to decide whether it should exist at all lives here
 * too (`gatewayDeployed`, the error count), because with real URLs a tab is
 * reachable by typing it. A page that only renders when its tab button is drawn
 * is not guarded; it just was not reachable before.
 */
export interface InfraState {
    loading: boolean;
    refreshing: boolean;
    nodes: NodeInfo[];
    edges: GatewayEdge[];
    links: GatewayLink[];
    routeCount: number;
    onlineEdges: number;
    errors: FlatServiceError[];
    /** What customers run. Null until the overview reports it. */
    customers: CustomerSummary | null;
    /** The feature flag: the gateway is configured. */
    gatewayEnabled: boolean;
    /** The flag AND something actually deployed behind it. */
    gatewayDeployed: boolean;
    onlineEdgesList: GatewayEdge[];
    /** Players on every server now, per their own list pings. Null until the first reading. */
    playersOnline: number | null;
    /** When that number was last read successfully (ms epoch), for the freshness label. */
    playersAt: number | null;
    /** When the last poll finished, success or not; re-renders the freshness label. */
    playersCheckedAt: number;
    refresh: (manual?: boolean) => void;
}

const Ctx = createContext<InfraState | null>(null);

export function useInfra(): InfraState {
    const v = useContext(Ctx);
    if (!v) throw new Error('useInfra must be used inside InfraProvider');
    return v;
}

const POLL_MS = 10000;
// Faster than the overview on purpose: it is one small read, and "how many are
// playing right now" is the number people watch.
const PLAYERS_POLL_MS = 5000;

export function InfraProvider({ gatewayEnabled, children }: { gatewayEnabled: boolean; children: React.ReactNode }) {
    const [data, setData] = useState<Omit<InfraState, 'loading' | 'refreshing' | 'gatewayEnabled' | 'gatewayDeployed' | 'onlineEdgesList' | 'refresh' | 'playersOnline' | 'playersAt' | 'playersCheckedAt'> | null>(null);
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const timer = useRef<ReturnType<typeof setInterval> | null>(null);

    const fetchData = useCallback(async (manual = false) => {
        if (manual) setRefreshing(true);
        try {
            const res = await getInfrastructureOverview();
            if (res && res.success !== false) {
                let nodes = (res.nodes || []) as NodeInfo[];
                if (nodes.length === 0) {
                    // The overview can come back without nodes while the node
                    // list itself is fine; falling back keeps the page from
                    // claiming an empty fleet.
                    try {
                        const nodesRes = await getNodes();
                        if (Array.isArray(nodesRes)) nodes = nodesRes as NodeInfo[];
                    } catch {
                        // Leave it empty rather than failing the whole screen.
                    }
                }
                setData({
                    nodes,
                    edges: res.edges || [],
                    links: res.links || [],
                    routeCount: res.routeCount ?? 0,
                    onlineEdges: res.onlineEdges ?? 0,
                    customers: res.customers ?? null,
                    // Core has always sent this and the view used to drop it,
                    // which took six components' diagnostics off every screen.
                    errors: flattenServiceErrors(res.errors),
                });
            }
        } catch {
            // A failed poll leaves the last good data on screen.
        } finally {
            setLoading(false);
            if (manual) setRefreshing(false);
        }
    }, []);

    const [players, setPlayers] = useState<{ count: number; at: number } | null>(null);
    const [playersCheckedAt, setPlayersCheckedAt] = useState(0);

    useEffect(() => {
        let cancelled = false;
        const poll = async () => {
            // Nobody is looking at a hidden tab; the visibility listener below
            // catches up as soon as somebody is.
            if (document.visibilityState === 'hidden') return;
            try {
                const res = await getPlayersOnline();
                if (!cancelled && res && res.success !== false && typeof res.playersOnline === 'number') {
                    setPlayers({ count: res.playersOnline, at: Date.now() });
                }
            } catch {
                // Keep the last number; the freshness label stops saying "Live".
            } finally {
                if (!cancelled) setPlayersCheckedAt(Date.now());
            }
        };
        const onVisible = () => { if (document.visibilityState === 'visible') void poll(); };
        void poll();
        const id = setInterval(() => { void poll(); }, PLAYERS_POLL_MS);
        document.addEventListener('visibilitychange', onVisible);
        return () => {
            cancelled = true;
            clearInterval(id);
            document.removeEventListener('visibilitychange', onVisible);
        };
    }, []);

    useEffect(() => {
        void fetchData();
        timer.current = setInterval(() => { void fetchData(); }, POLL_MS);
        return () => { if (timer.current) clearInterval(timer.current); };
    }, [fetchData]);

    const edges = data?.edges ?? [];
    const links = data?.links ?? [];
    const value: InfraState = {
        loading,
        refreshing,
        nodes: data?.nodes ?? [],
        edges,
        links,
        routeCount: data?.routeCount ?? 0,
        onlineEdges: data?.onlineEdges ?? 0,
        customers: data?.customers ?? null,
        errors: data?.errors ?? [],
        gatewayEnabled,
        // Enabled AND something behind it. A tab that renders an empty gateway
        // is a screen claiming a backend that is not there.
        gatewayDeployed: gatewayEnabled && (edges.length > 0 || links.length > 0),
        onlineEdgesList: edges.filter(e => e.status === 'online'),
        playersOnline: players?.count ?? null,
        playersAt: players?.at ?? null,
        playersCheckedAt,
        refresh: fetchData,
    };

    return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export type { InfrastructureData, NodeInfo };
