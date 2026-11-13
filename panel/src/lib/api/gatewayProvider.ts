// The gateway provider settings (admin), the managed link kit of one node, and
// the provider's usage for this month.
//
// The key is write-only end to end: no answer carries it, and a save that
// leaves the field empty sends no `key` at all, which keeps the stored one.
// The managed link kit is the exception that proves the rule - it IS a secret
// (the link's key), so callers hold it in component state only.

import { API_URL, getAuthHeader } from '@/lib/api/core';
import { handleUnauthorized } from '@/lib/api/session';
import { providerErrorText } from '@/lib/gatewayProvider';

export interface ProviderAccount {
    userId?: string;
    product?: string;
    managed?: boolean;
    entitlementSource?: string;
    suspended?: boolean;
    scopes?: string[];
    // Limits convention: null = no cap, 0 = none, n = the cap.
    limits?: { maxLinks?: number | null; maxRoutes?: number | null; maxCustomDomains?: number | null };
}

export interface ProviderProblem {
    code: string;
    message?: string;
}

export interface ProviderSettings {
    mode: 'local' | 'remote';
    url: string;
    keySet: boolean;
    keyFromEnv: boolean;
    urlFromEnv: boolean;
    keyUnreadable?: boolean;
    outbox?: { open: number; oldestAt?: string };
    account?: ProviderAccount;
    error?: ProviderProblem;
}

/** One answer of this module. On failure `code` and `message` say why. */
export type Answer<T> =
    | ({ success: true; status: number } & T)
    | { success: false; status?: number; code?: string; message: string };

const TOO_MANY = 'Too many attempts, try again in a minute.';

// Core answers refusals in two shapes: {message, code} from the settings
// handlers and {error, code} from the gateway gate. handleResponse in core.ts
// keeps only `message`, which would lose both the code and the gate's text.
async function answer<T>(res: Response, tooManyText?: string): Promise<Answer<T>> {
    if (handleUnauthorized(res)) return { success: false, status: 401, message: 'Session expired' };
    let data: Record<string, unknown> = {};
    try {
        data = await res.json();
    } catch {
        return { success: false, status: res.status, message: `Request failed (${res.status})` };
    }
    if (res.ok && data.success !== false) return { ...(data as T), success: true, status: res.status };
    const code = typeof data.code === 'string' ? data.code : undefined;
    if (res.status === 429 && tooManyText) return { success: false, status: 429, code, message: tooManyText };
    const text = typeof data.message === 'string' && data.message
        ? data.message
        : typeof data.error === 'string' && data.error
            ? data.error
            : code ? providerErrorText(code) : `Request failed (${res.status})`;
    return { success: false, status: res.status, code, message: text };
}

async function call<T>(path: string, init?: RequestInit, tooManyText?: string): Promise<Answer<T>> {
    try {
        const res = await fetch(`${API_URL}${path}`, {
            ...init,
            headers: { ...getAuthHeader(), ...(init?.body ? { 'Content-Type': 'application/json' } : {}) },
        });
        return await answer<T>(res, tooManyText);
    } catch (err) {
        if (init?.signal?.aborted || (err instanceof DOMException && err.name === 'AbortError')) {
            return { success: false, code: 'timeout', message: 'No answer in time, so the test was stopped.' };
        }
        return { success: false, message: 'Connection failed' };
    }
}

export const getGatewayProvider = () => call<ProviderSettings>('/settings/gateway/provider');

export interface ProviderSaveBody {
    mode: 'local' | 'remote';
    url?: string;
    key?: string;
}

/** What the form holds, and what was loaded, for working out what to send. */
export interface ProviderForm {
    mode: 'local' | 'remote';
    url: string;
    key: string;
    savedUrl: string;
    urlFromEnv: boolean;
    keyFromEnv: boolean;
}

const normUrl = (u: string) => u.trim().replace(/\/+$/, '');

/**
 * The save request. An unchanged URL and an empty key are left OUT: Core keeps
 * an omitted value and clears one sent as "". Values set by the environment
 * are never sent, because Core refuses any attempt to change them.
 */
export function providerSaveBody(f: ProviderForm): ProviderSaveBody {
    const body: ProviderSaveBody = { mode: f.mode };
    // The URL and key fields are hidden in local mode, so whatever they hold
    // is not something the operator is looking at.
    if (f.mode === 'local') return body;
    if (!f.urlFromEnv && normUrl(f.url) !== normUrl(f.savedUrl)) body.url = normUrl(f.url);
    if (!f.keyFromEnv && f.key.trim() !== '') body.key = f.key.trim();
    return body;
}

/** The test request: the same rule without the mode. Empty means "the one in force". */
export function providerTestBody(f: ProviderForm): { url?: string; key?: string } {
    const { mode: _mode, ...rest } = providerSaveBody({ ...f, mode: 'remote' });
    void _mode;
    return rest;
}

export const saveGatewayProvider = (body: ProviderSaveBody) =>
    call<ProviderSettings>('/settings/gateway/provider', { method: 'POST', body: JSON.stringify(body) }, TOO_MANY);

export interface ProviderTestResult {
    ok: boolean;
    account?: ProviderAccount;
    error?: ProviderProblem;
}

export const testGatewayProvider = (body: { url?: string; key?: string }, signal?: AbortSignal) =>
    call<ProviderTestResult>('/settings/gateway/provider/test', { method: 'POST', body: JSON.stringify(body), signal }, TOO_MANY);

export interface ProviderUsage {
    period?: string;
    edgeBytes?: number;
    relayBytes?: number;
    final?: boolean;
    pool?: { includedGb: number; maxPurchaseGb: number | null; usedBytes: number; percent: number } | null;
}

export const getProviderUsage = () => call<ProviderUsage>('/gateway/provider/usage');

export interface ManagedLinkKit {
    linkId: string;
    linkKey: string;
    coreUrl: string;
    network?: string;
    networkKnown?: boolean;
    networkHint?: string;
    online?: boolean | null;
}

/** Mints the kit on first call. The answer holds the link's key. */
export const getManagedLink = (nodeId: number) =>
    call<ManagedLinkKit>(`/nodes/${nodeId}/managed-link`, { cache: 'no-store' });
