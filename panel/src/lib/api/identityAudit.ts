import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

// The identity audit trail: what has happened to ACCOUNTS. Registrations,
// email and username changes, role and permission changes, 2FA resets and
// deletions.
//
// It was written since the audit work and read by nothing - no route, no
// screen - so the record an operator needs after removing an account was
// reachable only through a database shell. That included the deletion row
// added for exactly that question.
export interface IdentityAuditEvent {
    id: number;
    eventType: string;
    actorUserId?: string;
    actorName?: string;
    targetUserId?: string;
    targetName?: string;
    metadata?: Record<string, unknown>;
    ipAddress?: string;
    userAgent?: string;
    createdAt: string;
}

export interface IdentityAuditResponse {
    success: boolean;
    events?: IdentityAuditEvent[];
    message?: string;
}

export async function listIdentityAudit(
    opts?: { eventType?: string; targetUserId?: string; limit?: number },
): Promise<IdentityAuditResponse> {
    try {
        const p = new URLSearchParams();
        if (opts?.eventType) p.set('eventType', opts.eventType);
        if (opts?.targetUserId) p.set('targetUserId', opts.targetUserId);
        p.set('limit', String(opts?.limit ?? 100));
        const res = await fetch(`${API_URL}/admin/audit/identity?${p.toString()}`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as IdentityAuditResponse;
    } catch (err) {
        return handleError(err) as IdentityAuditResponse;
    }
}
