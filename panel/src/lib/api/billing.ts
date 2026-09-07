import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export type BillingStatus = 'active' | 'past_due' | 'suspended';

export interface BillingSettings {
    gracePeriod: string;
    r2Retention: string;
    nodeRetention: string;
    // A tri-state limit as a string: "" is unset, "unlimited" is a decided
    // no-cap, a number is that cap including 0. Read and written through
    // limitFromSetting / limitToSetting, never by hand.
    // Plain quantities per purchased unit, not tri-state limits: an unlimited
    // included allowance would be free infinite storage, and an unlimited
    // bookable one is the open bill its ceiling exists to prevent.
    r2IncludedGb: string;
    r2BookableGb: string;
    presignTtlNodeMin: string;
    presignTtlByonMin: string;
    paymentUrl: string;
}

/** The settings-table sentinel for "no cap at all", mirroring services.LimitUnlimited. */
export const LIMIT_UNLIMITED = 'unlimited';

/**
 * Reads an operator-typed limit out of the settings table into what LimitField
 * speaks: null is no cap, a number is the cap, and 0 is a real cap of none.
 *
 * An UNSET setting reads as null rather than 0. The two are opposite answers -
 * "nobody has capped this" against "they may store nothing" - and Core used to
 * hand the panel a "0" for unset, which the next save wrote back as a real cap
 * of none for every tenant.
 */
export function limitFromSetting(raw: string): number | null {
    if (raw === '' || raw === LIMIT_UNLIMITED) return null;
    const n = Number(raw);
    return Number.isFinite(n) && n >= 0 ? Math.trunc(n) : null;
}

/** The inverse, and the only writer. null stores the word, never an empty string. */
export function limitToSetting(v: number | null): string {
    return v === null ? LIMIT_UNLIMITED : String(v);
}

// Per-user retention + limit overrides. Empty string on a spec / null on a numeric
// field means "fall back to the platform default". A 0 is a real cap of NONE, not
// unlimited - services.r2QuotaGB reads it that way and a per-user 0 used to block
// nothing at all.
export interface UserBillingOverrides {
    gracePeriod: string;
    r2Retention: string;
    nodeRetention: string;
    r2QuotaGb: number | null;
    maxNodes: number | null;
    maxLinks: number | null;
    trafficEdgeGb: number | null;
    trafficRelayGb: number | null;
    trafficCombinedGb: number | null;
}

// Admin read of a single tenant's billing state plus the platform defaults the
// override fields fall back to (shown as placeholders in the UI).
export interface UserBillingAdmin {
    success: boolean;
    status: BillingStatus;
    graceUntil?: string | null;
    suspendedAt?: string | null;
    overrides: UserBillingOverrides;
    defaults: { gracePeriod: string; r2Retention: string; nodeRetention: string; r2QuotaGb: string };
    message?: string;
}

// MyTrafficStatus is how close the tenant is to the point where their traffic
// stops being free. Absent (undefined) on a self-hosted install, where nothing is
// metered and there is nothing to warn about.
export interface MyTrafficStatus {
    // The pool the tenant is CLOSEST to losing, not a sum. A total is the one
    // number that cannot stop anybody: somebody inside three allowances and past
    // the fourth is stopped by the fourth, and a banner reading 40% next to a
    // halted server is worse than no banner.
    usedGb: number;
    ceilingGb: number;
    // Uncapped upward: someone at 300% is shown 300%, not a reassuring 100%.
    pct: number;
    // false means reaching the ceiling STOPS their services instead of billing
    // them - which is the part the banner has to say out loud.
    billingEnabled: boolean;
    // The highest threshold ANY pool has reached: 0, 80, 90 or 100.
    warn?: number;
    // Every allowance the tenant is judged against. Player traffic is per
    // region; file transfers hold one pool for all of them.
    pools?: TrafficPool[];
}

// TrafficPool is one allowance. includedGb null means nothing is configured for
// it, which is NOT a limit of zero: it is not capped and not billed.
export interface TrafficPool {
    kind: string;   // "edge" (player traffic) | "relay" (file transfers)
    region: string; // "*" when the pool is not per region
    usedGb: number;
    includedGb: number | null;
    pct: number;
    warn: number;
    // How the pool's usage splits by product: "byon" for a server on a machine
    // the tenant owns, "route" for a protected address pointing at their own
    // server, "" for rows written before the split existed.
    //
    // BYTES, unlike everything else here, and the name says so: a share under a
    // gigabyte truncates to 0, and two of those beside a total of 1 GB reads as
    // a bug rather than as rounding.
    //
    // A breakdown, never a second allowance. The included traffic is granted per
    // unit HELD and pooled across products, so these describe who filled the
    // pool and are judged against nothing.
    byProductBytes?: Record<string, number>;
}

// getMyBilling returns the caller's lifecycle state for the banner.
export async function getMyBilling(): Promise<{ success: boolean; status?: BillingStatus; graceUntil?: string | null; paymentUrl?: string; traffic?: MyTrafficStatus | null }> {
    try {
        const res = await fetch(`${API_URL}/me/billing`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function getBillingSettings(): Promise<{ success: boolean } & Partial<BillingSettings> & { message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/billing`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

export async function setBillingSettings(s: BillingSettings): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/billing`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(s),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Admin: set a tenant's lifecycle status (active | past_due | suspended).
export async function setUserBillingStatus(userId: string, status: BillingStatus): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/billing`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ status }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Admin: read a tenant's full billing state + platform defaults for the modal.
export async function getUserBilling(userId: string): Promise<UserBillingAdmin> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/billing`, { headers: getAuthHeader() });
        return handleResponse(res) as any;
    } catch (err) { return handleError(err) as any; }
}

// RetentionOverridesInput is the subset the /billing-overrides endpoint accepts
// (the limit overrides go to /limit-overrides via setUserLimitOverrides).
export interface RetentionOverridesInput {
    gracePeriod: string;
    r2Retention: string;
    nodeRetention: string;
    r2QuotaGb: number | null;
}

// Admin: set a tenant's per-user retention overrides (empty string / null clears one).
export async function setUserBillingOverrides(userId: string, o: RetentionOverridesInput): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/admin/users/${userId}/billing-overrides`, {
            method: 'PATCH',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(o),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
