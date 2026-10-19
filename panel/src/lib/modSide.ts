// Where a mod runs, from Modrinth's project-level client_side / server_side
// ("required" | "optional" | "unsupported" | "unknown"). Mirrors modrinth.com's
// Environment filter and card labels (packages/ui/src/utils/search.ts and
// components/project/card/ProjectCardEnvironment.vue in modrinth/code), mapped
// from its v3 environments onto the v2 pair that our search proxy returns.

/** The two independent toggles; neither on means no filter. */
export interface EnvFilter {
    client: boolean;
    server: boolean;
}

export type EnvParam = 'client' | 'server' | 'both';

export interface ModSides {
    client_side?: string;
    server_side?: string;
}

const runs = (v?: string) => v === 'required' || v === 'optional';
const notNeeded = (v?: string) => v === 'optional' || v === 'unsupported';

/** The query value Core turns into facets, or undefined for no filter. */
export function envParam(f: EnvFilter): EnvParam | undefined {
    if (f.client && f.server) return 'both';
    if (f.client) return 'client';
    if (f.server) return 'server';
    return undefined;
}

/**
 * Client: runs on the client and the server does not need it. Server: the
 * reverse. Both: runs on either side. Core's facets say the same, but Modrinth
 * matches them per version, so a hit is checked again here.
 */
export function matchesEnv(f: EnvFilter, p: ModSides): boolean {
    switch (envParam(f)) {
        case 'client':
            return runs(p.client_side) && notNeeded(p.server_side);
        case 'server':
            return runs(p.server_side) && notNeeded(p.client_side);
        case 'both':
            return runs(p.client_side) && runs(p.server_side);
        default:
            return true;
    }
}

/** modrinth.com's card label, or null where Modrinth shows none. */
export function envLabel(p: ModSides): string | null {
    const c = p.client_side;
    const s = p.server_side;
    if (c === 'optional' && s === 'optional') return 'Client or server';
    if (c === 'required' && s === 'required') return 'Client and server';
    if (runs(c) && notNeeded(s)) return 'Client';
    if (runs(s) && notNeeded(c)) return 'Server';
    return null;
}
