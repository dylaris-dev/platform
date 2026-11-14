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
    // Absent when nobody is on the address right now.
    players?: UserGatewayPlayers;
}

/** Play sessions right now as the edges count them, a few seconds old. */
export interface UserGatewayPlayers {
    java: number;
    bedrock: number;
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
    // The account's total over its addresses; absent from a Core older than it.
    players?: UserGatewayPlayers;
    // Daily peaks, each a lower bound; absent from a Core older than them.
    // day is the server's UTC day (YYYY-MM-DD) the windows are measured against.
    peaks?: { day: string; today: PeakWindow; last30Days: PeakWindow; days: PeakDay[] };
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

/** "3 Java, 1 Bedrock", an edition with nobody left out; "None" at zero. */
export function playersLabel(p?: UserGatewayPlayers): string {
    const parts = [p?.java ? `${p.java} Java` : '', p?.bedrock ? `${p.bedrock} Bedrock` : ''].filter(Boolean);
    return parts.length ? parts.join(', ') : 'None';
}

/** The highest concurrent players over some UTC days, each column its own maximum. */
export interface PeakWindow {
    java: number;
    bedrock: number;
    total: number;
}

/** One UTC day's peak; day is YYYY-MM-DD. */
export interface PeakDay extends PeakWindow {
    day: string;
}

/** "Peak today: at least 4 (3 Java, 1 Bedrock)". Sampled once a minute, so "at least". */
export function peakLine(label: string, w: PeakWindow | undefined, editions: boolean): string {
    if (!w?.total) return `${label}: none recorded`;
    return editions ? `${label}: at least ${w.total} (${playersLabel(w)})` : `${label}: at least ${w.total}`;
}

/** The UTC day n days before day (YYYY-MM-DD). */
function daysBefore(day: string, n: number): string {
    return new Date(Date.parse(`${day}T00:00:00Z`) - n * 86_400_000).toISOString().slice(0, 10);
}

/** What the day list says for a day without a row. */
export const PEAK_NO_DATA = 'no data';

/**
 * The 30 daily totals ending today, oldest first; a day without a row is null,
 * never 0: peaks start on deploy day, so a zero there would be a claim nobody
 * measured. today is the SERVER's UTC day (peaks.day), never the browser
 * clock, so the chart and the list below it name the same days.
 */
export function peakSeries(days: PeakDay[], today: string): (number | null)[] {
    const byDay = new Map(days.map(d => [d.day, d.total]));
    return Array.from({ length: 30 }, (_, i) => byDay.get(daysBefore(today, 29 - i)) ?? null);
}

/** Every day from today back to the oldest row, newest first, a day without a
 *  row as peak null; empty without rows. Days before the oldest row are only
 *  in the chart. */
export function peakDayList(days: PeakDay[], today: string): { day: string; peak: PeakDay | null }[] {
    if (!days.length) return [];
    const byDay = new Map(days.map(d => [d.day, d]));
    const oldest = days.reduce((o, d) => (d.day < o ? d.day : o), days[0].day);
    const out: { day: string; peak: PeakDay | null }[] = [];
    for (let n = 0; n < 30; n++) {
        const day = daysBefore(today, n);
        if (day < oldest) break;
        out.push({ day, peak: byDay.get(day) ?? null });
    }
    return out;
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
    peaks: 'No players recorded yet.',
    customDomains: 'No own domains.',
    regions: 'No traffic this month.',
    beamKeys: 'No beam keys.',
    apiKeys: 'No API keys with gateway access.',
} as const;
