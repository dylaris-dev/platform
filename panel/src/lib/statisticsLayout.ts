// How Infrastructure > Statistics folds its numbers into its charts.

/** overview.fleet as Core sends it. */
export interface FleetSummary {
    nodes: {
        platform: { total: number; online: number };
        external: { total: number; online: number };
        byon: { total: number; online: number };
    };
    customers: { byon: number; routeOnly: number; total: number };
    routeOnlyRoutes: number;
    /** Null when Core could not count them. */
    users: number | null;
}

export interface FleetTile {
    label: string;
    value: string;
    sub: string;
}

/**
 * The fleet row, always six tiles in a fixed order so the grid has no holes:
 * a figure Core could not produce reads as a dash, not as a missing tile.
 */
export function fleetTiles(f: FleetSummary): FleetTile[] {
    const nodes = (c: { total: number; online: number }) => ({ value: `${c.online}/${c.total}`, sub: 'online' });
    return [
        { label: 'Platform nodes', ...nodes(f.nodes.platform) },
        { label: 'External nodes', ...nodes(f.nodes.external) },
        { label: 'BYON nodes', ...nodes(f.nodes.byon) },
        {
            label: 'Customers',
            value: String(f.customers.total),
            // Total counts an account holding both kinds once, so the two parts
            // can add up to more than it.
            sub: `${f.customers.byon} BYON, ${f.customers.routeOnly} route-only`,
        },
        { label: 'Route-only routes', value: String(f.routeOnlyRoutes), sub: 'configured' },
        { label: 'Registered users', value: f.users === null ? '-' : String(f.users), sub: 'accounts' },
    ];
}

/**
 * Each headline goes onto the chart of the same metric, when that chart is on
 * screen. The rest come back in their original order, to be shown compactly
 * rather than dropped: a headline from another group is still worth a glance.
 */
export function attachHeadlines<H extends { metric: string }>(
    chartMetrics: string[],
    headlines: H[],
): { onChart: Record<string, H>; rest: H[] } {
    const shown = new Set(chartMetrics);
    const onChart: Record<string, H> = {};
    const rest: H[] = [];
    for (const h of headlines) {
        if (shown.has(h.metric) && !onChart[h.metric]) onChart[h.metric] = h;
        else rest.push(h);
    }
    return { onChart, rest };
}
