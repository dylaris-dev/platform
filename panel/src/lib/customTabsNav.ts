// The custom tabs page's sidebar, as data: which servers show in which group,
// what a search matches, and how a tab is addressed in the URL. Kept out of the
// page so it can be tested without a DOM.
//
// The groups mirror the server sidebar: the reader's own servers, the ones they
// were invited to, and - for an admin who switched admin mode on - every other
// server they can reach. A server only ever appears in one group.

export interface NavServer {
    id: number;
    name: string;
    uuid: string;
    owner?: string;
    ownerId?: string;
    role?: string;
    invitedHere?: boolean;
}

export interface NavTab {
    id: number;
    name: string;
}

export type NavGroup = 'mine' | 'invited' | 'admin';

/** Which sidebar group a server belongs in. */
export function navGroupOf(s: NavServer): NavGroup {
    if (s.role === 'invited' || s.role === 'inherited' || (s.role === 'admin' && s.invitedHere)) return 'invited';
    if (s.role === 'admin') return 'admin';
    return 'mine';
}

/**
 * Whether a server (with its tabs) matches a search: server name, uuid, owner
 * name or owner id, or one of its tab names. Case-insensitive; an empty query
 * matches everything.
 */
export function navMatches(s: NavServer, tabs: NavTab[], query: string): boolean {
    const q = query.trim().toLowerCase();
    if (!q) return true;
    const hay = [s.name, s.uuid, s.owner ?? '', s.ownerId ?? '', ...tabs.map(t => t.name)];
    return hay.some(h => h.toLowerCase().includes(q));
}

/**
 * The groups to render, each with its servers in the given order. The admin
 * group is empty unless adminMode is on.
 */
export function navGroups<S extends NavServer>(
    servers: S[], tabsOf: (id: number) => NavTab[], query: string, adminMode: boolean,
): Record<NavGroup, S[]> {
    const out: Record<NavGroup, S[]> = { mine: [], invited: [], admin: [] };
    for (const s of servers) {
        const tabs = tabsOf(s.id);
        if (tabs.length === 0 || !navMatches(s, tabs, query)) continue;
        const g = navGroupOf(s);
        if (g === 'admin' && !adminMode) continue;
        out[g].push(s);
    }
    return out;
}

/** The URL query that opens one tab: ?server=<id>&tab=<id>. */
export function tabHref(serverId: number, tabId: number): string {
    return `/tabs?server=${serverId}&tab=${tabId}`;
}

/** The tab a URL names, or null for a missing or malformed one. */
export function parseTabParams(server: string | null, tab: string | null): { serverId: number; tabId: number } | null {
    const s = Number(server), t = Number(tab);
    if (!server || !tab || !Number.isInteger(s) || !Number.isInteger(t) || s <= 0 || t <= 0) return null;
    return { serverId: s, tabId: t };
}
