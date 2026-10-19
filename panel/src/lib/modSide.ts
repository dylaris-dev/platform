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

// ----- The server Content tab's model -----
// A server owner does not ask "where does it run" but "does it run HERE, and
// must my players install it too". Every mod falls in one bucket by its
// project-level sides; Core's server_only / client_server / server_runs facets
// say the same per version, so hits are checked again here.

export type ServerBucket = 'server_only' | 'client_server' | 'client_only';

/** The selected button; null = none, which shows everything a server can run. */
export type ServerBucketFilter = Exclude<ServerBucket, 'client_only'> | null;

export type ServerSideParam = 'server_only' | 'client_server' | 'server_runs';

/** The bucket, or null when Modrinth does not say enough to place it. */
export function serverBucket(p: ModSides): ServerBucket | null {
    if (p.server_side === 'unsupported') return 'client_only';
    if (!runs(p.server_side)) return null;
    if (p.client_side === 'required') return 'client_server';
    if (notNeeded(p.client_side)) return 'server_only';
    return null;
}

export function serverBucketLabel(b: ServerBucket): string {
    switch (b) {
        case 'server_only': return 'Server only';
        case 'client_server': return 'Client + Server';
        case 'client_only': return 'Client only';
    }
}

/** The tag's tooltip, or null where there is no tag. */
export function serverBucketTitle(p: ModSides): string | null {
    switch (serverBucket(p)) {
        case 'server_only':
            return p.client_side === 'optional'
                ? "Players don't need it, but may add it on their side."
                : "Players don't need it.";
        case 'client_server': return 'Players must install it too.';
        case 'client_only': return 'Does nothing on a server and can stop it from starting; players install it themselves';
        default: return null;
    }
}

/** The query value for Core: no button still drops what cannot run on a server. */
export function serverSideParam(f: ServerBucketFilter): ServerSideParam {
    return f ?? 'server_runs';
}

/**
 * Browse never shows a client-only mod: it is useless or crashing on a server.
 * Installed keeps it whatever is selected, so its warning tag stays seen. A mod
 * Modrinth does not place shows only while no button is selected.
 */
export function matchesServerBucket(f: ServerBucketFilter, p: ModSides, opts: { browse: boolean }): boolean {
    const b = serverBucket(p);
    if (b === 'client_only') return !opts.browse;
    return f === null || b === f;
}
