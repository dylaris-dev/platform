// panel client for user-owned API keys. Plaintext is only ever
// returned once on creation; subsequent reads (list) carry hash metadata
// without the key value.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface APIKeyScope {
    servers: string[];
    permissions: string[];
}

export interface APIKey {
    id: number;
    userId: string;
    name: string;
    scope: APIKeyScope;
    lastUsedAt?: string;
    expiresAt?: string;
    revokedAt?: string;
    ratePerMin: number;
    createdAt: string;
}

export interface CreateAPIKeyInput {
    name: string;
    servers: string[];
    permissions: string[];
    ratePerMin?: number;
    // Days until the key stops working; 0 or absent = never (at most 365).
    expiresInDays?: number;
    // Core re-authenticates this one: the key it mints authenticates by its own
    // hash, so it keeps working after the password change that kills every
    // session. code is only required when the account has 2FA.
    password: string;
    code?: string;
}

export interface CreateAPIKeyResponse {
    success: boolean;
    apiKey?: APIKey;
    plaintext?: string;
    message?: string;
}

// What the caller may actually mint, so the page never offers a choice the
// operator gate would refuse at create time.
export interface APIKeyOptions {
    // False when the operator has turned user keys off (admins are exempt).
    enabled: boolean;
    // Null means the operator set no whitelist, i.e. no extra restriction. An
    // empty array would mean "nothing allowed" - the backend deliberately sends
    // null for the first case, so do not normalise it away.
    allowedCaps: string[] | null;
    // Every capability a key may carry at all. Ones that manage access
    // (API keys, roles, members) never ride on a key.
    keyCaps?: string[];
}

export async function listAPIKeys(): Promise<{ success: boolean; keys?: APIKey[]; options?: APIKeyOptions; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/api-keys`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function createAPIKey(input: CreateAPIKeyInput): Promise<CreateAPIKeyResponse> {
    try {
        const res = await fetch(`${API_URL}/me/api-keys`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function revokeAPIKey(id: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/api-keys/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Roll replaces the secret and keeps the old one working for 24 h. Like create,
// Core re-authenticates it. The new key is only ever returned here.
export async function rollAPIKey(id: number, password: string, code?: string): Promise<{ success: boolean; key?: string; previousValidUntil?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/api-keys/${id}/roll`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ password, code }),
        });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}
