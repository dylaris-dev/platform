import { API_URL, getAuthHeader, handleResponse, handleError } from './core';
import { User, Node, AppModule } from './types';

// --- USERS ---
export async function getUsers() {
    try {
        const res = await fetch(`${API_URL}/users`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function createUser(user: Partial<User>) {
    try {
        const res = await fetch(`${API_URL}/users`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(user)
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function deleteUser(id: string) {
    try {
        const res = await fetch(`${API_URL}/users/${id}`, { method: 'DELETE', headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// --- NODES ---
// scope narrows the list server-side: 'placement' returns what this caller may
// actually put a server on, which is not the whole fleet even for an operator.
export async function getNodes(scope?: 'external' | 'byon' | 'placement' | 'fleet') {
    try {
        const res = await fetch(`${API_URL}/nodes${scope ? `?scope=${scope}` : ''}`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function createNode(node: Partial<Node>) {
    try {
        const res = await fetch(`${API_URL}/nodes`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(node)
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// --- MODULES ---
export async function getModules() {
    try {
        const res = await fetch(`${API_URL}/modules`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function createModule(module: Partial<AppModule>) {
    try {
        const res = await fetch(`${API_URL}/modules`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(module)
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function deleteModule(id: number) {
    try {
        const res = await fetch(`${API_URL}/modules/${id}`, { method: 'DELETE', headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}