// Direct-mode server domains: the admin DNS surface (Settings > DNS) and the
// per-server domain list. Shapes are the ones Core implements in
// core/handlers/domains. Credentials are write-only: no response carries one,
// `hasCredentials` is all a screen gets to know.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface DnsKindField {
    key: string;
    label: string;
    secret: boolean;
    optional: boolean;
}

export interface DnsKind {
    id: string;
    label: string;
    // false: the provider cannot list zones, so they are typed in by hand.
    zoneListing: boolean;
    fields: DnsKindField[];
}

export interface DnsZoneDomain {
    // server_domains row, for deliberate removal
    id: number;
    fqdn: string;
    serverId: number;
    serverName: string;
    createdBy: string;
    createdByName: string;
    // The creator no longer may use the zone. The record stays; only a new
    // subdomain is blocked.
    accessLost: boolean;
}

export interface DnsZone {
    id: number;
    name: string;
    manual: boolean;
    // false: the provider's zone listing no longer names it. Kept because
    // domains or Core's records still use it; no new subdomain goes in.
    listed: boolean;
    // "user:<id>" and "role:<id>"
    access: string[];
    domains: DnsZoneDomain[];
}

export interface DnsAccount {
    id: number;
    name: string;
    provider: string;
    hasCredentials: boolean;
    lastError: string;
    lastErrorAt: string;
    zones: DnsZone[];
}

export interface DnsAdmin {
    success: boolean;
    message?: string;
    status?: number;
    enabled: boolean;
    intervalSeconds: number;
    kinds: DnsKind[];
    accounts: DnsAccount[];
    // "unlimited" or a number string
    limits: { perUser: string; perServer: string };
}

interface Plain { success: boolean; message?: string; code?: string; status?: number }

async function call<T extends Plain>(path: string, method: string, body?: unknown): Promise<T> {
    try {
        const res = await fetch(`${API_URL}${path}`, {
            method,
            headers: body === undefined ? getAuthHeader() : { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: body === undefined ? undefined : JSON.stringify(body),
        });
        return (await handleResponse(res)) as T;
    } catch (err) {
        return handleError(err) as T;
    }
}

// Blank fields are left out, not sent empty: on an edit a missing field means
// "keep the stored value", and that must not hinge on how Core reads ''.
export function buildCredentialBody(fields: Record<string, string>): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(fields)) if (v.trim() !== '') out[k] = v;
    return out;
}

export interface DnsAccountInput {
    name: string;
    provider: string;
    credentials: Record<string, string>;
}

export const getDnsAdmin = () => call<DnsAdmin>('/admin/dns', 'GET');

export const createDnsAccount = (b: DnsAccountInput) =>
    call<Plain & { account?: DnsAccount }>('/admin/dns/accounts', 'POST', { ...b, credentials: buildCredentialBody(b.credentials) });

export const updateDnsAccount = (id: number, b: DnsAccountInput) =>
    call<Plain & { account?: DnsAccount }>(`/admin/dns/accounts/${id}`, 'PUT', { ...b, credentials: buildCredentialBody(b.credentials) });

export const deleteDnsAccount = (id: number) =>
    call<Plain & { orphanedNames?: string[] }>(`/admin/dns/accounts/${id}`, 'DELETE');

export const testDnsAccount = (id: number) => call<Plain>(`/admin/dns/accounts/${id}/test`, 'POST');

export const refreshDnsZones = (id: number) =>
    call<Plain & { zones?: { id: number; name: string }[] }>(`/admin/dns/accounts/${id}/zones/refresh`, 'POST');

export const addManualDnsZone = (id: number, name: string) =>
    call<Plain & { zones?: { id: number; name: string }[] }>(`/admin/dns/accounts/${id}/zones`, 'POST', { name });

export const setDnsZoneAccess = (zoneId: number, principals: string[]) =>
    call<Plain & { access?: string[] }>(`/admin/dns/zones/${zoneId}/access`, 'PUT', { principals });

export interface DnsSettingsInput {
    enabled?: boolean;
    intervalSeconds?: number;
    perUser?: string;
    perServer?: string;
}

export const saveDnsSettings = (b: DnsSettingsInput) => call<Plain>('/admin/dns/settings', 'PUT', b);

// ---- per server ----

export type DomainState = 'grey' | 'yellow' | 'green' | 'paused';

export interface ServerDomain {
    id: number;
    fqdn: string;
    mode: 'manual' | 'managed';
    zoneId: number | null;
    label: string;
    state: DomainState;
    problems: string[];
    observedAt: string;
    createdBy: string;
    createdAt: string;
}

export interface ServerDomains {
    success: boolean;
    message?: string;
    status?: number;
    domains: ServerDomain[];
    zones: { id: number; name: string }[];
    // max / serverMax null = no limit
    limits: { used: number; max: number | null; serverUsed: number; serverMax: number | null };
    // "ip:port", or "" when the server has no direct address yet
    directAddress: string;
    // Core's reconcile interval; absent from a Core older than this field.
    intervalSeconds?: number;
}

export type AddServerDomainBody =
    | { mode: 'manual'; fqdn: string }
    | { mode: 'managed'; zoneId: number; label: string };

export const getServerDomains = (serverId: string) =>
    call<ServerDomains>(`/servers/${encodeURIComponent(serverId)}/domains`, 'GET');

export const addServerDomain = (serverId: string, body: AddServerDomainBody) =>
    call<Plain & { domain?: ServerDomain }>(`/servers/${encodeURIComponent(serverId)}/domains`, 'POST', body);

export const deleteServerDomain = (serverId: string, domainId: number) =>
    call<Plain>(`/servers/${encodeURIComponent(serverId)}/domains/${domainId}`, 'DELETE');
