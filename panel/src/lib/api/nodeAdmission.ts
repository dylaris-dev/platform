// P0b-5 admin API: node admission config (join/IP mode + CIDRs), per-node
// reset-pairing, and the enroll-token surface. Mirrors the featureFlags.ts
// fetch/auth-header pattern (auth token key: authToken || token).

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';
import type { AdmissionCIDR, NodeEnrollToken } from '@/lib/api/types';

export async function getNodeAdmission(): Promise<{ success: boolean; joinMode?: string; ipMode?: string; cidrs?: AdmissionCIDR[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/node-admission`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; joinMode?: string; ipMode?: string; cidrs?: AdmissionCIDR[]; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function updateNodeAdmission(payload: { joinMode: string; ipMode: string }): Promise<{ success: boolean; joinMode?: string; ipMode?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/node-admission`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return (await handleResponse(res)) as { success: boolean; joinMode?: string; ipMode?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function addAdmissionCIDR(payload: { cidr: string; label: string }): Promise<{ success: boolean; cidr?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/node-admission/cidrs`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return (await handleResponse(res)) as { success: boolean; cidr?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function deleteAdmissionCIDR(id: string): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/node-admission/cidrs/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/**
 * Clears the node's secret and cuts its live Redis access.
 *
 * It no longer hands back a token. A node holding the cluster secret re-pairs
 * itself within seconds; any other node then shows up under connection
 * attempts, where it is admitted with one click instead of an environment
 * variable and a redeploy.
 */
export async function resetNodePairing(nodeId: number): Promise<{ success: boolean; note?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/nodes/${nodeId}/reset-pairing`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; note?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/**
 * One connection Core is refusing.
 *
 * Only `peerIp` is observed - Core reads it off the socket. Everything else is
 * what the machine SAID about itself, before any proof was checked, so the UI
 * labels it as reported and the approval is bound to the address rather than to
 * anything on this list.
 */
export interface NodeJoinAttempt {
    nodeToken: string;
    nodeName: string;
    displayName: string;
    peerIp: string;
    reportedPublicIp: string;
    reportedPrivateIps: string;
    hostname: string;
    cpuCores: number;
    cpuModel: string;
    memoryBytes: number;
    releaseVersion: string;
    reason: string;
    attempts: number;
    firstSeenAt: string;
    lastSeenAt: string;
    approvedUntil?: string;
    approvedFromIp?: string;
    approvedBy?: string;
}

export async function listNodeJoinAttempts(): Promise<{ success: boolean; attempts?: NodeJoinAttempt[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/nodes/join-attempts`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; attempts?: NodeJoinAttempt[]; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/** Admits this identity from the address the attempt came from, briefly. */
export async function approveNodeJoinAttempt(nodeToken: string): Promise<{ success: boolean; note?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/nodes/join-attempts/${encodeURIComponent(nodeToken)}/approve`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; note?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/** Drops the row. It comes back if the machine keeps trying - this is not a block. */
export async function dismissNodeJoinAttempt(nodeToken: string): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/nodes/join-attempts/${encodeURIComponent(nodeToken)}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function mintEnrollToken(payload: { label: string; expiresDays: number }): Promise<{ success: boolean; token?: string; grpcTlsFingerprint?: string; note?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/nodes/enroll-token`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ label: payload.label, expiresDays: payload.expiresDays }),
        });
        return (await handleResponse(res)) as { success: boolean; token?: string; grpcTlsFingerprint?: string; note?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function listEnrollTokens(): Promise<{ success: boolean; tokens?: NodeEnrollToken[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/nodes/enroll-token`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; tokens?: NodeEnrollToken[]; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function revokeEnrollToken(id: string): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/nodes/enroll-token/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return (await handleResponse(res)) as { success: boolean; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}
