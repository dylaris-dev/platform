// The tenant's own machine: reading what removing it would destroy, and
// removing it. Both answer only for a node the caller owns; see
// core/handlers/node_self_service.go for why this is a separate surface from
// the capability-gated admin routes rather than a relaxed gate on them.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/** One server that would go with the machine. */
export interface MyNodeServer {
    id: number;
    name: string;
    uuid: string;
    /** Every install on it, by name. Read from the database, so it is still
     *  correct while the machine is already offline. */
    subServers: string[];
    /** The one currently booted, when there is one. */
    activeSubServer?: string;
}

export interface MyNodeContents {
    success: boolean;
    message?: string;
    node?: { id: number; name: string; status: string };
    servers?: MyNodeServer[];
}

export async function getMyNodeContents(nodeId: number): Promise<MyNodeContents> {
    try {
        const res = await fetch(`${API_URL}/me/nodes/${nodeId}/contents`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as MyNodeContents;
    } catch (err) {
        return handleError(err) as MyNodeContents;
    }
}

/**
 * Remove the caller's own machine.
 *
 * withServers is explicit rather than inferred: the two reasons to remove a
 * machine are "I am moving it" and "I am done with it", and only the second
 * wants the worlds gone. Core refuses while servers remain unless it is set.
 */
export async function deleteMyNode(nodeId: number, withServers: boolean): Promise<{ success: boolean; message?: string }> {
    try {
        const q = withServers ? '?servers=delete' : '';
        const res = await fetch(`${API_URL}/me/nodes/${nodeId}${q}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/** A connection Core is refusing for the caller's machine. */
export interface MyNodeJoinAttempt {
    /** Full fingerprint of the key it presented; "" for none. */
    presentedKey?: string;
    hostname?: string;
    reason?: string;
    attempts?: number;
    lastSeenAt?: string;
    approvedUntil?: string | null;
}

/** The first 16 hex digits in groups of four: what the node logs at start. */
export function shortFingerprint(fp: string): string {
    return fp.length < 16 ? fp : `${fp.slice(0, 4)}-${fp.slice(4, 8)}-${fp.slice(8, 12)}-${fp.slice(12, 16)}`;
}

type Ok = { success: boolean; message?: string; note?: string };

/** Take away this machine's login; it then knocks with a new key to be admitted. */
export async function resetMyNodePairing(nodeId: number): Promise<Ok> {
    try {
        const res = await fetch(`${API_URL}/me/nodes/${nodeId}/reset-pairing`, { method: 'POST', headers: getAuthHeader() });
        return (await handleResponse(res)) as Ok;
    } catch (err) {
        return handleError(err) as Ok;
    }
}

export async function getMyNodeJoinAttempt(nodeId: number): Promise<{ success: boolean; attempt?: MyNodeJoinAttempt | null; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/nodes/${nodeId}/join-attempt`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; attempt?: MyNodeJoinAttempt | null };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/** Admit exactly the key the owner was shown; refused if another is knocking by now. */
export async function admitMyNode(nodeId: number, fingerprint: string): Promise<Ok> {
    try {
        const res = await fetch(`${API_URL}/me/nodes/${nodeId}/admit`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ fingerprint }),
        });
        return (await handleResponse(res)) as Ok;
    } catch (err) {
        return handleError(err) as Ok;
    }
}
