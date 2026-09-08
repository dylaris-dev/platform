import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

export interface BackupSummary {
    name: string;
    size: number;
    createdAt: string;
    counts?: Record<string, number>;
}

export interface MigrationStatusResponse {
    success: boolean;
    mainCounts: Record<string, number>;
    externalConfigured: boolean;
    message?: string;
}

export interface RestoreInitResponse {
    success: boolean;
    token: string;
    cooldownSeconds: number;
    confirmationPhrase: string;
    message?: string;
}

export async function getTicketMigrationStatus() {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/migration/status`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function testTicketDBConnection(url: string, signal?: AbortSignal) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/migration/test-connection`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ url }),
            signal,
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function dryRunTicketMigration(url: string) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/migration/dry-run`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ url }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function executeTicketMigration(url: string) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/migration/execute`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ url }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// ── Backups ─────────────────────────────────────────────────────────

export async function createTicketBackup() {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/backup`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function listTicketBackups() {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/backups`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export function downloadTicketBackupURL(name: string): string {
    return `${API_URL}/admin/tickets/backups/${encodeURIComponent(name)}/download`;
}

export async function deleteTicketBackup(name: string) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/backups/${encodeURIComponent(name)}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// ── Restore (Danger Zone) ───────────────────────────────────────────

export async function initTicketRestore(name: string) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/restore/init`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ name }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function executeTicketRestore(payload: { token: string; totpCode: string; confirmationPhrase: string }) {
    try {
        const res = await fetch(`${API_URL}/admin/tickets/restore/execute`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
