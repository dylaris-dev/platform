// first-run setup wizard client. Two endpoints:
//   GET  /api/setup/status - what mode are we in?
//   POST /api/setup/admin  - create the first admin
//
// Both are open routes (no auth header needed) - the setup-lock middleware
// short-circuits /api/setup/* even in Fresh-Install mode where every other
// API route returns 503 setup_required.

import { API_URL, handleResponse, handleError } from '@/lib/api/core';

export type SetupMode = 'fresh_install' | 'lost_admin' | 'complete';

export interface SetupStatus {
    success: boolean;
    mode: SetupMode;
    adminSecretConfigured: boolean;
    /** env SETUP as configured on Core. Reported separately from `open` so the
     *  page can say WHY the wizard is closed rather than only that it is. */
    setupEnabled: boolean;
    /** Whether there is a working form here at all. Computed by the same gate
     *  the create endpoint uses, so the page and the handler cannot disagree. */
    open: boolean;
    /** Ask for the red "no admin token configured" banner: the door is open and
     *  without ADMIN_SECRET nothing can come through it. */
    needsSecretWarning: boolean;
    frontendUrl?: string;
    message?: string;
}

export interface SetupTOTPInfo {
    secret: string;
    code: string;
}

export interface SetupAdminRequest {
    username: string;
    password: string;
    adminSecret?: string;
    totp?: SetupTOTPInfo;
}

export interface SetupAdminUser {
    id: string;
    username: string;
    isAdmin: boolean;
}

export interface SetupAdminResponse {
    success: boolean;
    error?: string;
    message?: string;
    user?: SetupAdminUser;
    token?: string;
    /** Present once, when the wizard set up 2FA. */
    backupCodes?: string[];
}

// The authed layout awaits getSetupStatus before it renders anything, so this
// one request decides whether the panel appears at all. fetch has NO default
// timeout, and a host that accepts the connection and then never answers does
// not reject - it just leaves the promise pending. The catch below was written
// for "hard transport failure" and never saw that case, so an unreachable API
// left the entire panel sitting in its skeleton shell: no error, no retry,
// nothing in the console, indistinguishable from a slow page.
//
// Reproduced on the testbed: the panel is served at localhost:25510 and calls
// localhost:25500, Chromium resolves that to ::1, and only the IPv4 path is
// actually wired - so every call hangs. That is an environment quirk, but the
// dead page it produced is not: any unreachable API does the same.
//
// A hang is a transport failure with extra steps, so time it out and let it
// land in the catch that already knows what to do.
const SETUP_STATUS_TIMEOUT_MS = 8000;

export async function getSetupStatus(): Promise<SetupStatus> {
    try {
        const res = await fetch(`${API_URL}/setup/status`, {
            signal: AbortSignal.timeout(SETUP_STATUS_TIMEOUT_MS),
        });
        return (await handleResponse(res)) as any;
    } catch (err) {
        // On hard transport failure, fall back to "complete" so a flaky
        // network doesn't lock the user into the wizard forever. The next
        // user action that fetches anything else will reveal whether the
        // backend is actually reachable.
        // Closed, not open: a transport failure must not render a working-looking
        // admin-creation form.
        return {
            success: false, mode: 'complete', adminSecretConfigured: false,
            setupEnabled: false, open: false, needsSecretWarning: false,
        };
    }
}

export async function createFirstAdmin(req: SetupAdminRequest): Promise<SetupAdminResponse> {
    try {
        const res = await fetch(`${API_URL}/setup/admin`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(req),
        });
        return (await handleResponse(res)) as any;
    } catch (err) {
        return handleError(err) as any;
    }
}
