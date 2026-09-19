// Technic Platform proxy client. Core fetches and caches the metadata
// (/api/technic/*); nothing here talks to Technic directly.

import { API_URL, getAuthHeader } from '@/lib/api/core';

export interface TechnicSearchHit {
    slug: string;
    name: string;
    iconUrl: string;
}

/** What the node will install: the author's server pack, or the client pack. */
export type TechnicVariant = 'server' | 'client-zip' | 'client-solder' | 'none';

export interface TechnicPack {
    slug: string;
    displayName: string;
    author: string;
    minecraft: string;
    version: string;
    iconUrl: string;
    platformUrl: string;
    variant: TechnicVariant;
    builds?: { recommended: string; latest: string; list: string[] };
}

/** Either the data or the message Core sent (e.g. Technic rate limiting). */
export type TechnicResult<T> = { ok: true; data: T } | { ok: false; message: string };

async function technicGet<T>(url: string): Promise<TechnicResult<T>> {
    try {
        const res = await fetch(url, { headers: getAuthHeader() });
        const body = await res.json().catch(() => null);
        if (!res.ok) {
            return { ok: false, message: body?.message || `Technic request failed (${res.status})` };
        }
        return { ok: true, data: body as T };
    } catch {
        return { ok: false, message: 'Could not reach the panel API.' };
    }
}

export function searchTechnic(q: string): Promise<TechnicResult<{ packs: TechnicSearchHit[] }>> {
    return technicGet(`${API_URL}/technic/search?q=${encodeURIComponent(q)}`);
}

export function getTechnicPack(slug: string): Promise<TechnicResult<TechnicPack>> {
    return technicGet(`${API_URL}/technic/pack/${encodeURIComponent(slug)}`);
}
