// panel client for user-owned SSH keys. They sign the account in to SFTP,
// which is the only SFTP login an account with 2FA has: the password alone is
// refused there because SFTP cannot ask for the second factor.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface SSHKey {
    id: number;
    name: string;
    publicKey: string;
    fingerprint: string;
    createdAt: string;
}

export interface AddSSHKeyInput {
    name: string;
    publicKey: string;
    // Re-authenticated like an API key create: a key is a login that survives
    // the password change that kills every session. code only with 2FA.
    password: string;
    code?: string;
}

export async function listSSHKeys(): Promise<{ success: boolean; keys?: SSHKey[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/ssh-keys`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function addSSHKey(input: AddSSHKeyInput): Promise<{ success: boolean; fingerprint?: string; note?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/ssh-keys`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(input),
        });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function deleteSSHKey(id: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/ssh-keys/${id}`, {
            method: 'DELETE',
            headers: getAuthHeader(),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
