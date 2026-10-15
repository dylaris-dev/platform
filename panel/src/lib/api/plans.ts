import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

// An operator's node or link ceiling over the purchase: null follows what was
// bought, 'unlimited' lifts it, a number replaces it. It never gives access -
// that is a grant - and the store never writes it, so a renewal leaves it alone.
export type AdminCeiling = null | 'unlimited' | number;

// Per-user limit overrides. The traffic fields are warn-only thresholds on the
// platform convention: null leaves them unset (no limit), 0 is none.
export interface LimitOverrides {
    maxNodes: AdminCeiling;
    maxLinks: AdminCeiling;
    trafficEdgeGb: number | null;
    trafficRelayGb: number | null;
    trafficCombinedGb: number | null;
}

export async function setUserLimitOverrides(userId: string, o: LimitOverrides): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/limit-overrides`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(o),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
