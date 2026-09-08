// Where the long-term statistics are written.
//
// One answer: a database of its own, on a PostgreSQL carrying TimescaleDB. The
// Core database used to be the other, at hour resolution, and both it and the
// mode field that chose between them are gone - a second resolution nobody
// could convert between was a choice whose only lasting effect was history that
// turned out coarser than the operator thought.
//
// A target is therefore named or it is not. Not named means nothing is
// recorded, and that is also how recording is switched off.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface MetricsDBTarget {
    host: string;
    port: string;
    dbName: string;
    user: string;
    /** Write-only. The GET never returns it; see passwordSet. */
    password?: string;
    sslMode: string;
}

/** What is being written RIGHT NOW, which is not always what is configured. */
export interface MetricsDBActive {
    recording: boolean;
    resolution?: 'minute';
}

/**
 * One save: whether to record, and where.
 *
 * They travel together because they ARE one decision - recording starts the
 * moment `enabled` is true and the first bucket lands at the resolution the
 * target implies, with no way to backfill or convert afterwards.
 */
export interface MetricsDBRequest extends MetricsDBTarget {
    enabled: boolean;
    /**
     * The database has NO password, as opposed to the field being left alone.
     *
     * A blank field already means "keep what is stored", so without this there
     * is no way to take a password back off once one was saved - and none is
     * the correct setting for a database reached over a private network.
     */
    noPassword: boolean;
}

export interface MetricsDBSettings extends MetricsDBRequest {
    /**
     * A password is stored. Distinct from an empty field, which here means
     * there genuinely is none - a metrics database on a private network can
     * legitimately run without one.
     */
    passwordSet: boolean;
    active: MetricsDBActive;
}

/** Result of the test button. `severity` drives the colour, not `ok`. */
export interface MetricsDBTestResult {
    ok: boolean;
    severity: 'ok' | 'warning' | 'error';
    /** How far the attempt got - see lib/connectionTest.ts. */
    stage?: string;
    message: string;
    timescale?: boolean;
    version?: string;
}

export const emptyMetricsDBTarget: MetricsDBRequest = {
    enabled: false,
    noPassword: true,
    host: '',
    port: '5432',
    dbName: '',
    user: '',
    password: '',
    sslMode: 'disable',
};

export async function getMetricsDB(): Promise<{ success: boolean; settings?: MetricsDBSettings; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/metrics-db`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; settings?: MetricsDBSettings; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; settings?: MetricsDBSettings; message?: string };
    }
}

export async function saveMetricsDB(
    target: MetricsDBRequest,
): Promise<{ success: boolean; settings?: MetricsDBSettings; warning?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/metrics-db`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json', ...getAuthHeader() },
            body: JSON.stringify(target),
        });
        return (await handleResponse(res)) as { success: boolean; settings?: MetricsDBSettings; warning?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

export async function testMetricsDB(
    target: MetricsDBTarget,
    signal?: AbortSignal,
): Promise<{ success: boolean } & Partial<MetricsDBTestResult> & { message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/metrics-db/test`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', ...getAuthHeader() },
            body: JSON.stringify(target),
            signal,
        });
        return (await handleResponse(res)) as { success: boolean } & Partial<MetricsDBTestResult>;
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}

/**
 * Whether the form is complete enough to save or test.
 *
 * A password is deliberately NOT required - the reference deployment runs its
 * statistics database without one, reachable only from Core on a private
 * network. Everything else is: there is no fallback database left to catch a
 * half-filled form.
 */
export function metricsDBIncomplete(t: MetricsDBTarget): string | null {
    if (!t.host.trim()) return 'Enter the host of the statistics database.';
    if (!t.dbName.trim()) return 'Enter the database name.';
    if (!t.user.trim()) return 'Enter the user to connect as.';
    const port = Number(t.port.trim());
    if (!Number.isInteger(port) || port < 1 || port > 65535) return 'Port must be a number between 1 and 65535.';
    return null;
}
