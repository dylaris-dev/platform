"use client";

import { useEffect, useState } from 'react';
import RoutesPanel from '@/views/infrastructure/RoutesPanel';
import { getInfrastructureOverview, type GatewayEdge } from '@/lib/api';
import { SkeletonTable } from '@/components/Skeleton';
import { useAppData } from '@/lib/AppDataContext';
import { MANAGED_GATEWAY_NOTE } from '@/lib/gatewayProvider';

/**
 * The gateway routes, under Admin.
 *
 * They used to be a tab on Infrastructure, among the machines. A route is not a
 * machine: it is a record of what the gateway serves, which is the same kind of
 * list as Users and All Servers.
 *
 * This page fetches the overview itself rather than reading the infrastructure
 * context, which does not reach here. It needs two things from it: the online
 * edges, which the panel shows as the DNS targets a route can point at, and
 * whether a gateway exists at all - a routes screen with no gateway behind it
 * would list nothing and explain nothing.
 */

export default function Page() {
    const [edges, setEdges] = useState<GatewayEdge[] | null>(null);
    const [links, setLinks] = useState<number>(0);
    const { gatewayVisibility } = useAppData();
    const local = gatewayVisibility.infrastructureGateway;

    useEffect(() => {
        if (!local) return;
        let cancelled = false;
        const load = () => {
            getInfrastructureOverview()
                .then(res => {
                    if (cancelled || !res || res.success === false) return;
                    setEdges((res.edges || []) as GatewayEdge[]);
                    setLinks((res.links || []).length);
                })
                .catch(() => { if (!cancelled) setEdges([]); });
        };
        load();
        // The route list refreshes itself; this only keeps the DNS-target card
        // current, so it polls at the slow cadence the overview is meant for.
        const t = setInterval(load, 10000);
        return () => { cancelled = true; clearInterval(t); };
    }, [local]);

    // The provider holds the route list; this Core's admin route API answers 409.
    if (!local) return <div className="card p-8 text-center text-(--base-06) text-sm">{MANAGED_GATEWAY_NOTE}</div>;

    if (edges === null) return <SkeletonTable rows={6} />;

    // Deployed means something is actually behind the gateway. An edge or a
    // link is enough; either one can serve a route.
    if (edges.length === 0 && links === 0) {
        return (
            <div className="card p-8 text-center text-(--base-06) text-sm">
                Routes are served by the gateway, and none is deployed. Enable gateway routing and deploy an edge first.
            </div>
        );
    }

    return <RoutesPanel onlineEdges={edges.filter(e => e.status === 'online')} />;
}
