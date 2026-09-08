import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

export type DBMigrationPhase =
    | 'preparing'
    | 'schema'
    | 'copying'
    | 'verifying'
    | 'done'
    | 'failed';

// DBTargetForm is the connection the admin fills in. dbType is the
// multiple-choice TARGET backend ('postgres' | 'timescaledb').
export interface DBTargetForm {
    host: string;
    port: string;
    user: string;
    password: string;
    dbName: string;
    sslMode: string;
    dbType: string;
}

export interface TableCopyResult {
    table: string;
    rows: number;
}

export interface TableVerify {
    table: string;
    sourceExists: boolean;
    targetExists: boolean;
    sourceRows: number;
    targetRows: number;
    ok: boolean;
}

export interface VerifyReport {
    ok: boolean;
    tables: TableVerify[];
    log: string[];
    checkedAt: string;
}

export interface DBTargetInfo {
    host: string;
    port: string;
    user: string;
    dbName: string;
    sslMode: string;
    dbType: string;
}

export interface DBMigrationJob {
    id: string;
    phase: DBMigrationPhase;
    startedBy: string;
    startedByName: string;
    startedAt: string;
    updatedAt: string;
    finishedAt?: string;
    target: DBTargetInfo;
    tablesTotal: number;
    tablesDone: number;
    currentTable: string;
    tables: TableCopyResult[];
    log: string[];
    error?: string;
    verify?: VerifyReport;
    maintenanceOn: boolean;
    stale: boolean;
}

export function dbMigrationInProgress(phase?: DBMigrationPhase): boolean {
    return phase === 'preparing' || phase === 'schema' || phase === 'copying' || phase === 'verifying';
}

export interface HypertableStatus {
    dbType: string;
    timescaleInstalled: boolean;
    isHypertable: boolean;
    eligibleForConversion: boolean;
    serverStatsRowsEstimate: number;
    recommendTimescale: boolean;
    recommendThreshold: number;
}

// GET current backend state: timescale extension, hypertable status, and whether
// we recommend switching to TimescaleDB.
export async function getHypertableStatus() {
    try {
        const res = await fetch(`${API_URL}/admin/db/hypertable`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// POST convert the existing plain server_stats table to a hypertable in place.
export async function convertHypertable() {
    try {
        const res = await fetch(`${API_URL}/admin/db/hypertable/convert`, {
            method: 'POST',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// GET shared job status (any admin sees the same live job).
export async function getDBMigration() {
    try {
        const res = await fetch(`${API_URL}/admin/db/migration`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// POST start the migration to the given target. Enables maintenance mode.
export async function startDBMigration(target: DBTargetForm) {
    try {
        const res = await fetch(`${API_URL}/admin/db/migration`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(target),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// POST probe the target connection; returns server version + whether the
// timescaledb extension is installed there.
export async function testDBMigrationConnection(target: DBTargetForm, signal?: AbortSignal) {
    try {
        const res = await fetch(`${API_URL}/admin/db/migration/test-connection`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(target),
            signal,
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// POST run the source-vs-target comparison on demand (manual Verify button).
export async function verifyDBMigration(target: DBTargetForm) {
    try {
        const res = await fetch(`${API_URL}/admin/db/migration/verify`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(target),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
