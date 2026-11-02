import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

export interface ServerAuditEvent {
    id: number;
    serverId: number;
    region: string;
    eventType: string;
    actorUserId?: string;
    actorName?: string;
    // Set when an API key acted; actorName is then the key's owner.
    actorApiKeyId?: number;
    actorApiKeyName?: string;
    targetUserId?: string;
    targetName?: string;
    metadata?: Record<string, unknown>;
    ipAddress?: string;
    userAgent?: string;
    createdAt: string;
}

export interface ServerAuditState {
    enabled: boolean;
    forceOn: boolean;
    effectiveOn: boolean;
    eventCount: number;
}

export interface AuditPolicy {
    serverRetentionDays: number;
}

export async function listServerAudit(serverId: number, opts: { eventType?: string; limit?: number; offset?: number } = {}) {
    try {
        const params = new URLSearchParams();
        if (opts.eventType) params.set('eventType', opts.eventType);
        if (opts.limit != null) params.set('limit', String(opts.limit));
        if (opts.offset != null) params.set('offset', String(opts.offset));
        const url = `${API_URL}/servers/${serverId}/audit${params.toString() ? '?' + params.toString() : ''}`;
        const res = await fetch(url, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function getServerAuditStatus(serverId: number) {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/audit/status`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function setServerAuditForce(serverId: number, forceOn: boolean) {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/audit/force`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ forceOn }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function getAuditPolicy() {
    try {
        const res = await fetch(`${API_URL}/admin/settings/audit`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function saveAuditPolicy(policy: AuditPolicy) {
    try {
        const res = await fetch(`${API_URL}/admin/settings/audit`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(policy),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
