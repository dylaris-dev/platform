// Pure helpers for the server Domains card and the header address.

import type { DomainState, ServerDomain } from '@/lib/api/serverDomains';
import type { Server } from '@/lib/api/types';

export type StatusTone = 'neutral' | 'warning' | 'accent';

const STYLES: Record<DomainState, { label: string; tone: StatusTone }> = {
    grey: { label: 'Not resolving yet', tone: 'neutral' },
    yellow: { label: 'Needs attention', tone: 'warning' },
    green: { label: 'Ready', tone: 'accent' },
    paused: { label: 'Paused: gateway routing', tone: 'neutral' },
};

export const statusStyle = (state: DomainState) => STYLES[state];

// The first green FQDN, else the node's ip:port. A domain that does not
// resolve to this server yet must never be what a player is told to type.
export function primaryAddress(domains: Pick<ServerDomain, 'fqdn' | 'state'>[], directAddress: string): string {
    return domains.find(d => d.state === 'green')?.fqdn ?? directAddress;
}

// domains.read / domains.write for the viewer. Owner and admin rows carry no
// permissions blob and count as owners (same rule as canManagePlayers).
export function domainAccess(server: Pick<Server, 'role' | 'permissions'>): { read: boolean; write: boolean } {
    if (server.role === 'demo') return { read: false, write: false };
    if (server.role !== 'invited' && server.role !== 'inherited') return { read: true, write: true };
    const p = server.permissions;
    return { read: !!p?.domains, write: !!p?.domainsWrite };
}

export function checkedAgo(observedAt: string, now: number): string {
    const t = Date.parse(observedAt);
    // Go's zero time parses fine but is year 1.
    if (!observedAt || Number.isNaN(t) || t < 0) return 'not checked yet';
    const s = Math.max(0, Math.round((now - t) / 1000));
    if (s < 60) return `checked ${s} s ago`;
    if (s < 3600) return `checked ${Math.floor(s / 60)} min ago`;
    return `checked ${Math.floor(s / 3600)} h ago`;
}

export const previewFqdn = (label: string, zone: string): string => {
    const l = label.trim().toLowerCase();
    return l && zone ? `${l}.${zone}` : '';
};
