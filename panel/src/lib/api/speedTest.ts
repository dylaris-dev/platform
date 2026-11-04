import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

// The backup storage speed test: a node uploads 64-512 MB to the storage and
// reports how fast that went. mine = the caller's own bucket (/api/me), whose
// test runs from a node their servers use or from their own machine.

export interface SpeedTestNode { id: number; name: string; online: boolean }
export interface SpeedTestResult {
    success: boolean;
    message?: string;
    mbit?: number;
    bytes?: number;
    durationMs?: number;
    /** The upload went to a local-network address. */
    lan?: boolean;
    node?: string;
}

const base = (storageId: number, mine: boolean) => `${API_URL}${mine ? '/me' : ''}/backup-storages/${storageId}`;

export async function listSpeedTestNodes(storageId: number, mine: boolean): Promise<{ success: boolean; nodes?: SpeedTestNode[]; message?: string }> {
    try {
        return await handleResponse(await fetch(`${base(storageId, mine)}/speedtest-nodes`, { headers: getAuthHeader() }));
    } catch (e) {
        return handleError(e);
    }
}

export async function runSpeedTest(storageId: number, mine: boolean, nodeId: number, sizeMb: number): Promise<SpeedTestResult> {
    try {
        return await handleResponse(await fetch(`${base(storageId, mine)}/speedtest`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ nodeId, sizeMb }),
        }));
    } catch (e) {
        return handleError(e);
    }
}

/** A suggested cap: 70% of the measured speed, so the storage is never filled. */
export function suggestedCap(mbit: number): number {
    return Math.max(1, Math.floor(mbit * 0.7));
}
