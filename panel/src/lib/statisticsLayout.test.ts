import { describe, it, expect } from 'vitest';
import { attachHeadlines, fleetTiles, type FleetSummary } from './statisticsLayout';

const fleet: FleetSummary = {
    nodes: {
        platform: { total: 3, online: 3 },
        external: { total: 1, online: 0 },
        byon: { total: 4, online: 2 },
    },
    customers: { byon: 3, routeOnly: 2, total: 4 },
    routeOnlyRoutes: 7,
    users: 120,
};

describe('fleetTiles', () => {
    it('renders every figure in a fixed order', () => {
        expect(fleetTiles(fleet)).toEqual([
            { label: 'Platform nodes', value: '3/3', sub: 'online' },
            { label: 'External nodes', value: '0/1', sub: 'online' },
            { label: 'BYON nodes', value: '2/4', sub: 'online' },
            { label: 'Customers', value: '4', sub: '3 BYON, 2 route-only' },
            { label: 'Route-only routes', value: '7', sub: 'configured' },
            { label: 'Registered users', value: '120', sub: 'accounts' },
        ]);
    });

    // A missing tile would leave a hole in the grid; a dash keeps it whole.
    it('keeps the users tile when Core could not count them', () => {
        const tiles = fleetTiles({ ...fleet, users: null });
        expect(tiles).toHaveLength(6);
        expect(tiles[5].value).toBe('-');
    });
});

describe('attachHeadlines', () => {
    const h = (metric: string) => ({ metric, value: 1 });

    it('puts a headline on the chart of the same metric', () => {
        const { onChart, rest } = attachHeadlines(
            ['platform.players_online', 'platform.servers_online'],
            [h('platform.players_online'), h('edge.restarts'), h('platform.servers_online')],
        );
        expect(Object.keys(onChart).sort()).toEqual(['platform.players_online', 'platform.servers_online']);
        expect(rest.map(r => r.metric)).toEqual(['edge.restarts']);
    });

    it('drops nothing when no chart matches', () => {
        const { onChart, rest } = attachHeadlines([], [h('a'), h('b')]);
        expect(onChart).toEqual({});
        expect(rest.map(r => r.metric)).toEqual(['a', 'b']);
    });
});
