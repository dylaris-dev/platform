import { API_URL, getAuthHeader, handleResponse, handleError } from './core';

// Store-linking client. The connect-store surface only exists when the hosted
// Core has the store ENV set (features.store); these calls 404 otherwise.

export interface StoreStatus {
    success: boolean;
    enabled: boolean;
    linked: boolean;
    email?: string;
    storeUrl?: string;
    message?: string;
}

// GET /store/status — linked state, read on demand from dylaris.com.
export async function getStoreStatus(): Promise<StoreStatus> {
    try {
        const res = await fetch(`${API_URL}/store/status`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as StoreStatus;
    } catch (err) {
        return handleError(err) as StoreStatus;
    }
}

// POST /store/link/start — mints a single-use token and returns the storefront
// redirect URL (dylaris.com/connect?token=...).
export async function startStoreLink(): Promise<{ success: boolean; redirectUrl?: string; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/store/link/start`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
        });
        return (await handleResponse(res)) as { success: boolean; redirectUrl?: string; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; redirectUrl?: string; message?: string };
    }
}

// The tenant's own subscription, usage and billing consent.
//
// Assembled by the store and passed through Core, which names the account from
// the session rather than from anything the caller sends. reachable=false is a
// real state and not an error: the panel keeps working while the storefront is
// quiet, and saying so beats rendering an account that looks empty.
export interface StoreUsageBlock {
    usedGb: number;
    includedGb: number;
    ceilingGb?: number;
    pct?: number;
}

export interface StoreBackupBlock extends StoreUsageBlock {
    ownStorageGb: number;
    bookableGb: number;
    blockGb: number;
    blocksUsed: number;
}

export interface StoreAccountSummary {
    success: boolean;
    enabled: boolean;
    reachable?: boolean;
    message?: string;
    linked?: boolean;
    email?: string;
    storeUrl?: string;
    subscribed?: boolean;
    // Whether an ADMIN GRANT is what entitles them. Core adds it; the storefront
    // cannot know, because a grant made in the panel creates no subscription
    // there. Without it a granted tenant was told their subscription was
    // missing, which describes their access as broken when it is simply not a
    // purchase.
    granted?: boolean;
    status?: string;
    nodes?: number;
    routeOnly?: number;
    stripe?: boolean;
    trafficMeterConfigured?: boolean;
    backupMeterConfigured?: boolean;
    trafficBillingEnabled?: boolean;
    backupBillingEnabled?: boolean;
    trafficCutoffAt?: string | null;
    // Per-unit prices read from Stripe, and the sentence each switch agrees
    // to. Absent when the store could not read the price.
    trafficPrice?: string;
    trafficConsentText?: string;
    backupPrice?: string;
    backupConsentText?: string;
    traffic?: StoreUsageBlock & { warn?: number; cutOff?: boolean };
    backup?: StoreBackupBlock;
}

// GET /store/account-summary
export async function getStoreAccountSummary(): Promise<StoreAccountSummary> {
    try {
        const res = await fetch(`${API_URL}/store/account-summary`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as StoreAccountSummary;
    } catch (err) {
        return handleError(err) as StoreAccountSummary;
    }
}

// POST /store/billing-consent - an omitted field is left alone, so the two
// switches never overwrite each other.
export async function setStoreBillingConsent(
    change: { traffic?: boolean; backup?: boolean },
): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/store/billing-consent`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(change),
        });
        return (await handleResponse(res)) as { success: boolean; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; message?: string };
    }
}
