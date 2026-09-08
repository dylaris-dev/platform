// read the public feature-toggle map exposed by /api/system/features.
// Used by the AppDataContext to drive panel banners + UI gating.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

export interface FeatureFlags {
    // The modpack subsystem. True means it exists at all; on its own that means
    // admins can author.
    modpacks: boolean;
    // End-user authoring. Separate from `modpacks` so the UI can tell "authoring
    // is closed on this platform" apart from "modpacks are off entirely".
    modpackAuthoring: boolean;
    tickets: boolean;
    // Raw platform flag. The panel ANDs this with the live routing mode, since
    // auto-move is only effective while the gateway is on.
    autoMove: boolean;
    // The shared file library. It is here rather than derived from the module
    // row because /library is a PAGE: a module row hides a navbar entry and
    // leaves the page reachable by URL.
    library: boolean;
    // BYON tenancy. Gates tenant-facing UI like the server transfer control.
    byon: boolean;
    // Store integration (dylaris.com). True only when the hosted Core has both
    // STORE_URL + STORE_SHARED_KEY set. Gates the connect-store button and the
    // demo account/server admin UI; false on a self-host/open-core build.
    store: boolean;
    // Gates the builder's share-link create UI. Existing links still show/copy/
    // revoke while modpacks is on; this only reflects the create toggle.
    shareLinks: boolean;
    // Whether modpack archives have anywhere to go. Separate from `modpacks`:
    // the subsystem can be switched on with no storage behind it, and until now
    // that only surfaced as an HTTP 424 at the end of building a pack.
    modpackStorage: boolean;
}

// Every field is optional on the way OUT: the Features screen is a set of tabs
// and each one saves only what it shows. Core leaves an absent flag alone, so a
// tab cannot revert a flag it does not render. A GET always fills them all.
export interface FeatureFlagsAdminPayload {
    tickets?: boolean;
    // The shared file library.
    library?: boolean;
    // The modpack subsystem. On its own it means admins can author.
    modpacks?: boolean;
    // Opens authoring to non-admin users. Requires `modpacks`; the backend folds
    // it to false when the subsystem is off, so the two can never disagree.
    modpackAuthoring?: boolean;
    // Write-only instruction, not stored state: what a change to
    // modpackAuthoring should do to users whose per-user flag an admin set BY
    // HAND. Omitted/false leaves those rows alone.
    applyAuthoringToManual?: boolean;
    autoMove?: boolean;
    byon?: boolean;
    // NO metrics flag. Long-term statistics are switched on by
    // /admin/settings/metrics-db, together with the database they record into:
    // the resolution is fixed the moment recording starts and nothing can be
    // backfilled, so the two have to be saved by one request. See metricsDb.ts.
    // Whether NON-ADMINS may hold an API key at all. Default off: a key is a
    // second credential class that outlives a session and is not covered by the
    // account's 2FA, so a fresh install does not start handing them out.
    userApiKeys?: boolean;
    // Comma-separated capability ids a non-admin may put on a key. EMPTY MEANS
    // NO EXTRA RESTRICTION, not "none" - the backend already stops a key from
    // exceeding what its creator holds.
    userApiKeyAllowedCaps?: string;
}

export async function getSystemFeatures(): Promise<{ success: boolean; features?: FeatureFlags; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/system/features`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as { success: boolean; features?: FeatureFlags; message?: string };
    } catch (err) {
        return handleError(err) as { success: boolean; features?: FeatureFlags; message?: string };
    }
}

// Admin-only GET — returns the same shape as the public /system/features map
// but with a body shape ready to round-trip back through PUT.
/** enabledTicketCategories rides along with the flags because the Tickets
 *  switch is a lie without it: a ticket must name an enabled category, so with
 *  zero of them the module accepts nothing while reporting itself as on. Absent
 *  when Core could not count them, which the screen treats as "say nothing"
 *  rather than as zero. */
type SystemFeaturesAdminResult = {
    success: boolean;
    features?: FeatureFlagsAdminPayload;
    enabledTicketCategories?: number;
    message?: string;
};

export async function getSystemFeaturesAdmin(): Promise<SystemFeaturesAdminResult> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/features`, { headers: getAuthHeader() });
        return (await handleResponse(res)) as SystemFeaturesAdminResult;
    } catch (err) {
        return handleError(err) as SystemFeaturesAdminResult;
    }
}

// Result of the admin PUT. usersChanged is how many per-user modpack rows the
// authoring toggle rewrote (0 when nothing needed changing, absent when the
// authoring flag did not move), so the UI can report the side effect instead of
// leaving the admin to guess what the switch touched.
export interface UpdateSystemFeaturesResult {
    success: boolean;
    features?: FeatureFlagsAdminPayload;
    usersChanged?: number;
    message?: string;
}

// Admin-only PUT — writes the bundle of platform-wide feature toggles in
// one round-trip. Backend invalidates the cache + publishes features.changed.
export async function updateSystemFeatures(payload: FeatureFlagsAdminPayload): Promise<UpdateSystemFeaturesResult> {
    try {
        const res = await fetch(`${API_URL}/admin/settings/features`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
        });
        return (await handleResponse(res)) as UpdateSystemFeaturesResult;
    } catch (err) {
        return handleError(err) as UpdateSystemFeaturesResult;
    }
}
