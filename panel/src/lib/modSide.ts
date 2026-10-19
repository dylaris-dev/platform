// Where a mod runs, from Modrinth's project-level client_side / server_side
// ("required" | "optional" | "unsupported" | "unknown").

export type ModSide = 'all' | 'server' | 'client';

export interface ModSides {
    client_side?: string;
    server_side?: string;
}

/**
 * "server": players do not need it (client optional or unsupported) and it
 * runs on a server. "client": players need it (client required) or it does
 * nothing on a server (server unsupported). Core's search facets say the same,
 * but Modrinth matches them per version, so a hit is checked again here.
 */
export function matchesSide(side: ModSide, p: ModSides): boolean {
    switch (side) {
        case 'server':
            return (p.client_side === 'optional' || p.client_side === 'unsupported')
                && (p.server_side === 'required' || p.server_side === 'optional');
        case 'client':
            return p.client_side === 'required' || p.server_side === 'unsupported';
        default:
            return true;
    }
}
