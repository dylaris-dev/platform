import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/**
 * Traffic allowances per (region, kind).
 *
 * A TB does not cost the same everywhere - Singapore is several times Nuremberg
 * at the same provider - so one flat allowance either prices the expensive
 * region at a loss or the rest at a discount.
 *
 * Both numbers follow the platform limit convention:
 *
 *   null - no limit at all
 *   0    - none
 *   n    - the cap, in GB
 *
 * For maxPurchaseGb that difference is the point: 0 is a region where extra
 * traffic cannot be bought at any price, and null is the opposite.
 */
export interface TrafficLimit {
    id: number;
    scope: string;  // "user_default" | "user:<uuid>"
    region: string;
    kind: string;   // "edge" (player traffic) | "relay" (file transfers)
    includedGb: number | null;
    maxPurchaseGb: number | null;
}

/** The traffic kinds, in the order they are shown. */
export const TRAFFIC_KINDS = ['edge', 'relay'] as const;
export type TrafficKind = (typeof TRAFFIC_KINDS)[number];

/**
 * The region a NON-regional kind stores its one row under.
 *
 * Must match services.TrafficRegionAny in Core, which is what actually resolves
 * a limit. A mismatch would store the number under a region the resolver never
 * asks about: a value an operator set, saw echoed back, and that enforces
 * nothing.
 */
export const TRAFFIC_REGION_ANY = '*';

/**
 * Whether a kind is capped per region.
 *
 * Player traffic is: it is measured at the edge that served it, and a terabyte
 * out of Singapore costs several times one out of Nuremberg. File transfers are
 * not: every beam relay is in eu-central, so they hold a single allowance.
 */
export function isRegionalKind(kind: string): boolean {
    // Must stay in step with services.RegionalKind in Core. Backups are
    // deliberately not a traffic kind at all: R2 charges nothing for ingress and
    // on a BYON node the bytes are the customer's own bandwidth, so there is
    // nothing of ours to cap. Their cost is STORAGE, capped by the R2 quota.
    return kind === 'edge';
}

/** Where a (region, kind) pair's limit actually lives. */
export function limitRegionFor(region: string, kind: string): string {
    return isRegionalKind(kind) ? region : TRAFFIC_REGION_ANY;
}

/**
 * What each kind means to an operator. The names in the database are the wire
 * names the producers write; these are what a person reads.
 */
export const KIND_LABELS: Record<string, string> = {
    edge: 'Player traffic',
    relay: 'File transfers',
    warp: 'Overlay',
};

export const KIND_HINTS: Record<string, string> = {
    edge: 'Measured at the edge that served the player, so it lands in the region the address routes through.',
    relay: 'Beam file transfers, measured at the relay that carried them - which is near the node, not near the player.',
    warp: 'The overlay. Nothing meters it yet; it carries the control plane rather than payload.',
};

/**
 * How a value is being set. The three states an operator means cannot be
 * expressed by a nullable number alone: "the scope above decides" and "decided
 * here, no limit" are both an absent number, and only one of them stops the
 * lookup.
 */
export type LimitMode = 'default' | 'unlimited' | 'custom';

export interface TrafficLimitWrite {
    scope: string;
    region: string;
    kind: string;
    includedMode: LimitMode;
    includedGb?: number;
    purchaseMode: LimitMode;
    purchaseGb?: number;
}

export const listTrafficLimits = async (): Promise<{ success: boolean; limits?: TrafficLimit[]; message?: string }> => {
    try {
        const res = await fetch(`${API_URL}/traffic-limits`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
};

/**
 * Writes one scope's answer for one (region, kind).
 *
 * "default" for BOTH values deletes the row on the server, which is not the
 * same as storing two nulls: deleting makes the next scope answer, storing null
 * answers "no limit" here and stops the walk. That is the difference an
 * operator relies on when undoing an override.
 */
export const setTrafficLimit = async (body: TrafficLimitWrite): Promise<{ success: boolean; message?: string }> => {
    try {
        const res = await fetch(`${API_URL}/traffic-limits`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
};

/** The mode a stored row is in, for rendering. A row that exists is never in
 *  "default" mode - its existence IS the decision. */
export function modeOf(value: number | null): Exclude<LimitMode, 'default'> {
    return value === null ? 'unlimited' : 'custom';
}

/** Turns a LimitField value plus "is this row set at all" into a write. */
export function writeFor(set: boolean, value: number | null): { mode: LimitMode; gb?: number } {
    if (!set) return { mode: 'default' };
    if (value === null) return { mode: 'unlimited' };
    return { mode: 'custom', gb: value };
}

// The speed beam and custom tabs slow to once a tenant without data overage
// has used its data allowance (Mbit/s, at least minMbit).
export async function getDataThrottle(): Promise<{ success: boolean; mbit?: number; minMbit?: number; defaultMbit?: number; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/traffic-limits/data-throttle`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
export async function setDataThrottle(mbit: number): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/traffic-limits/data-throttle`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ mbit }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
