// The admin view of one managed gateway customer (GET /api/admin/users/{id}/gateway):
// the request, and the pure helpers the tab renders with. Read-only; Core
// sends no secret in it (no link token, key hash or beam public key).

import { API_URL, getAuthHeader } from '@/lib/api/core';
import { handleUnauthorized } from '@/lib/api/session';
import { barPercent, providerGbToBytes, purchaseNote } from '@/lib/gatewayProvider';

export interface UserGatewayKit {
    linkId: string;
    name: string;
    region: string;
    createdAt: string;
    // null: Core could not ask, which is not "offline".
    online: boolean | null;
    caps: string[];
    retireAt?: string;
}

export interface UserGatewayRoute {
    domain: string;
    targetHost: string;
    targetPort: number;
    linkId: string;
    proxyProtocol: boolean;
    suspendedAt?: string;
    suspendedReason?: string;
}

export interface UserGatewayClaim {
    domain: string;
    state: string;
    attempts: number;
    checkedAt?: string;
    lapsesAt?: string;
}

export interface UserGatewayPool {
    includedGb: number;
    maxPurchaseGb: number | null;
    usedBytes: number;
    percent: number;
}

export interface UserGatewayUsage {
    period: string;
    edgeBytes: number;
    relayBytes: number;
    final?: boolean;
    regions: { region: string; kind: string; product: string; bytes: number }[];
    pool?: UserGatewayPool;
}

/** A block Core left out because the caller lacks its capability. */
export type HiddenBlock = 'billing' | 'usage' | 'apiKeys';

export interface UserGateway {
    userId: string;
    // Absent with "billing" in hidden (needs plans.read).
    product?: 'managed' | 'route_only' | 'none' | string;
    billingStatus?: string;
    overLimitSince?: string;
    // Limits convention: null = no cap, 0 = none, n = the cap.
    limits?: { maxLinks: number | null; maxRoutes: number | null; maxCustomDomains: number | null };
    kits: UserGatewayKit[];
    kitsTruncated: boolean;
    kitsUsed: number;
    kitsRetiring: number;
    routes: UserGatewayRoute[];
    routesTruncated: boolean;
    customDomains: UserGatewayClaim[];
    customDomainsTruncated: boolean;
    // Absent with "usage" in hidden (needs plans.read).
    usage?: UserGatewayUsage;
    beamKeys: { kid: string; createdAt: string; revokedAt?: string }[];
    beamKeysTruncated: boolean;
    // Absent with "apiKeys" in hidden (needs users.read).
    apiKeys?: { id: number; name: string; permissions: string[]; createdAt: string; lastUsedAt?: string; expiresAt?: string; revokedAt?: string }[];
    apiKeysTruncated?: boolean;
    hidden?: HiddenBlock[];
}

export type UserGatewayAnswer = { success: true; data: UserGateway } | { success: false; status?: number; message: string };

/** One request; a refusal carries the server's own sentence. */
export async function getUserGateway(userId: string): Promise<UserGatewayAnswer> {
    let res: Response;
    try {
        res = await fetch(`${API_URL}/admin/users/${encodeURIComponent(userId)}/gateway`, { headers: getAuthHeader(), cache: 'no-store' });
    } catch {
        return { success: false, message: 'Connection failed' };
    }
    if (handleUnauthorized(res)) return { success: false, message: 'Session expired' };
    let data: Record<string, unknown> = {};
    try {
        data = await res.json();
    } catch {
        return { success: false, message: `Request failed (${res.status})` };
    }
    return gatewayAnswerOf(res.ok, res.status, data);
}

/** The answer of one response body, split out so it is testable. */
export function gatewayAnswerOf(ok: boolean, status: number, data: Record<string, unknown>): UserGatewayAnswer {
    if (ok && data.success !== false) return { success: true, data: data as unknown as UserGateway };
    const text = typeof data.message === 'string' && data.message
        ? data.message
        : typeof data.error === 'string' && data.error ? data.error : `Request failed (${status})`;
    return { success: false, status, message: text };
}

/** The one line the tab shows instead of Core's "Forbidden". */
export const NOT_FOR_ROLE = 'Not available for your role.';

/** What the tab says for a failed load: a refusal reads neutrally. */
export function failureText(a: { status?: number; message: string }): string {
    return a.status === 403 ? NOT_FOR_ROLE : a.message;
}

const HIDDEN_TEXT: Record<HiddenBlock, string> = {
    billing: 'Plan, billing and limits are not shown for your role.',
    usage: 'Traffic is not shown for your role.',
    apiKeys: 'API keys are not shown for your role.',
};

/** The muted line for a block Core left out, or null when it is shown. */
export function hiddenNote(hidden: HiddenBlock[] | undefined, block: HiddenBlock): string | null {
    return hidden?.includes(block) ? HIDDEN_TEXT[block] : null;
}

const PRODUCT_LABELS: Record<string, string> = {
    managed: 'Managed gateway',
    route_only: 'Route-only',
    none: 'No gateway product',
};

export function productLabel(product: string): string {
    return PRODUCT_LABELS[product] ?? product;
}

/** A cap on the platform convention. */
export function limitText(limit: number | null | undefined): string {
    if (limit === null || limit === undefined) return 'No limit';
    if (limit === 0) return 'None';
    return String(limit);
}

export type KitState = { label: string; tone: 'success' | 'neutral' | 'warning' };

export function kitState(kit: Pick<UserGatewayKit, 'online' | 'retireAt'>): KitState {
    if (kit.retireAt) return { label: 'Being replaced', tone: 'warning' };
    if (kit.online === true) return { label: 'Online', tone: 'success' };
    if (kit.online === false) return { label: 'Offline', tone: 'neutral' };
    return { label: 'Unknown', tone: 'neutral' };
}

/** "Suspended" with the reason when there is one, or null for a live route. */
export function routeSuspendedLabel(route: Pick<UserGatewayRoute, 'suspendedAt' | 'suspendedReason'>): string | null {
    if (!route.suspendedAt) return null;
    return route.suspendedReason ? `Suspended: ${route.suspendedReason}` : 'Suspended';
}

/** The pool line under the bar, in the provider's decimal units. */
export function poolText(pool: UserGatewayPool | undefined, fmt: (bytes: number) => string): string | null {
    if (!pool) return null;
    const pct = barPercent(pool.percent);
    return `${pct}% of the pool used: ${fmt(pool.usedBytes)} of ${fmt(providerGbToBytes(pool.includedGb))} included. ${purchaseNote(pool.maxPurchaseGb, fmt)}`;
}

/** The notice under a list Core cut at its 200-item bound. */
export function truncatedNote(truncated: boolean, shown: number): string | null {
    return truncated ? `Showing the first ${shown}. This account has more.` : null;
}

/** Each section's empty-state line, so no section renders as a blank box. */
export const EMPTY_TEXT = {
    kits: 'No links. The account has not created a managed link yet.',
    routes: 'No addresses.',
    customDomains: 'No own domains.',
    regions: 'No traffic this month.',
    beamKeys: 'No beam keys.',
    apiKeys: 'No API keys with gateway access.',
} as const;
