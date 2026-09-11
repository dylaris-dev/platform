// What a BYON or route-only deploy snippet still needs from Core.
//
// The two overlay addresses used to be here as well. They now go straight to
// the machine's warp, which proxies them on fixed local ports, so no snippet
// carries an address any more and none has to be re-copied when the platform
// overlay is rebuilt.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface WarpDeployConfig {
    /** Stored overlay CIDR(s), or Core's detected value. "" = undetermined. */
    tunnelSubnets: string;
    /**
     * Core's gRPC certificate fingerprint while its control channel runs TLS,
     * "" while it runs plaintext. Absent from a Core that predates the field,
     * which a kit reads as "not known" rather than as plaintext.
     */
    grpcTlsFingerprint?: string;
    /**
     * The name a tenant points their OWN domain at, when the operator configured
     * one. Absent means custom domains have no published target and the panel
     * must not invent one - a wrong record is worse than no instruction.
     */
}

export async function getWarpDeployConfig(): Promise<{ success: boolean; config?: WarpDeployConfig; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/warp/deploy-config`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; config?: WarpDeployConfig; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}
