import { forgetSessionHint, purgeLegacyTokens } from '@/lib/api/sessionState';
import { API_URL, GATE_TIMEOUT_MS, getAuthHeader, handleResponse, handleError } from './core';

export const login = async (username: string, password: string, totpCode?: string) => {
  try {
    const body: Record<string, string> = { username, password };
    if (totpCode) body.totpCode = totpCode;
    const res = await fetch(`${API_URL}/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });

    // Read the body EXACTLY ONCE. A Response body is a one-shot stream: this
    // used to be read here for the 2FA/verification flags and then again inside
    // handleResponse, where the second read throws "body stream already read".
    // handleResponse treats a throw as "the body was not JSON" and reports
    // `Request failed (401)` - so every wrong password showed a status code
    // instead of Core's actual message. Do not reintroduce a second read.
    //
    // handleResponse is deliberately not used at all here: its 401 branch
    // treats the status as an expired session, and a failed LOGIN never is one.
    const data: any = await res.json().catch(() => null);

    if (res.ok) {
      // The token is NOT stored. Core set an HttpOnly cookie on this response
      // and the browser is already holding it; keeping a second, readable copy
      // would hand back exactly what the cookie exists to take away.
      //
      // It is still in the body, for the Beam desktop client and anything else
      // driving this API programmatically. The panel simply ignores it.
      return { success: true, ...(data ?? {}) };
    }

    // 401 with requires2FA flag is the "password OK, give me the TOTP code" path
    if (res.status === 401 && data?.requires2FA) {
      return { success: false, requires2FA: true, message: data.message };
    }

    // 403 covers the policy-driven gates: unverified email or
    // missing 2FA-setup when required. Surface the flags so the
    // login form can route to the right next step.
    if (res.status === 403 && data?.requiresVerification) {
      return { success: false, requiresVerification: true, email: data.email, message: data.message };
    }
    if (res.status === 403 && data?.requires2FASetup) {
      return {
        success: false,
        requires2FASetup: true,
        setupToken: data.setupToken,
        setupTokenExpires: data.setupTokenExpires,
        message: data.message,
      };
    }

    // Keep the status only when there was no parsable body to quote.
    return { success: false, message: data?.message || `Request failed (${res.status})` };
  } catch (err) {
    return handleError(err);
  }
};

// --- 2FA / TOTP ---

export const setupTOTP = async () => {
  const res = await fetch(`${API_URL}/auth/2fa/setup`, {
    method: 'POST',
    headers: getAuthHeader(),
  });
  return handleResponse(res);
};

// The password re-authenticates the account holder. Core requires it for a
// normal session; the forced-enrolment flow below is exempt, because its setup
// token is minted by a password login moments earlier (verifyTOTPWithToken).
export const verifyTOTP = async (secret: string, code: string, password: string) => {
  const res = await fetch(`${API_URL}/auth/2fa/verify`, {
    method: 'POST',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ secret, code, password }),
  });
  return handleResponse(res);
};

export const disableTOTP = async (password: string, code: string) => {
  const res = await fetch(`${API_URL}/auth/2fa/disable`, {
    method: 'POST',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ password, code }),
  });
  return handleResponse(res);
};

// Removing somebody's second factor leaves their password as the whole
// credential, so Core asks the acting administrator for their own. A DELETE
// carrying a body is unusual and deliberate on that side: the alternative was
// a second method for the same action.
export const adminResetTOTP = async (userId: string, reauth?: { password: string; code: string }) => {
  const res = await fetch(`${API_URL}/users/${userId}/2fa`, {
    method: 'DELETE',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ reauth }),
  });
  return handleResponse(res);
};

// regenerateBackupCodes — issues a fresh set of 10 single-use codes for an
// already-2FA-enabled user. Same defence-in-depth as disable: requires
// current password + a valid TOTP/backup code.
export const regenerateBackupCodes = async (password: string, code: string) => {
  const res = await fetch(`${API_URL}/auth/2fa/regenerate-backup-codes`, {
    method: 'POST',
    headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ password, code }),
  });
  return handleResponse(res);
};

export const get2FAStatus = async () => {
  const res = await fetch(`${API_URL}/auth/2fa/status`, {
    headers: getAuthHeader(),
  });
  return handleResponse(res);
};

// setupTOTPWithToken / verifyTOTPWithToken — variants of the regular setup
// flow that authenticate via an explicit Bearer token (the short-lived
// setup-token issued by the login endpoint when 2FA enrollment is forced).
// Necessary because at this point the user does NOT have a session JWT in
// localStorage yet — the regular getAuthHeader() would send nothing.
export const setupTOTPWithToken = async (setupToken: string) => {
  const res = await fetch(`${API_URL}/auth/2fa/setup`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${setupToken}` },
  });
  return handleResponse(res);
};

export const verifyTOTPWithToken = async (setupToken: string, secret: string, code: string) => {
  const res = await fetch(`${API_URL}/auth/2fa/verify`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${setupToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ secret, code }),
  });
  return handleResponse(res);
};

// Resolves to the user, or null when the session is genuinely not valid.
// REJECTS when the API could not be reached at all - and the difference
// matters: the only caller treats null as "log the user out".
//
// This used to catch everything and return null, so a Core that was down for a
// few seconds looked exactly like an expired token. Reproduced on the testbed:
// stopping Core and reloading the panel wiped BOTH localStorage tokens and
// dropped the user on /login, with a token that was still perfectly valid. A
// deploy logged out every open tab.
//
// The timeout is what makes the rejection reachable at all: a host that
// accepts the connection and never answers neither resolves nor rejects.
export const getProfile = async () => {
  const res = await fetch(`${API_URL}/auth/profile`, {
    method: 'GET',
    headers: getAuthHeader(),
    signal: AbortSignal.timeout(GATE_TIMEOUT_MS),
  });
  const data = await handleResponse(res);
  return data.success ? data.user : null;
};

export const updateProfile = async (data: any) => {
  try {
    const res = await fetch(`${API_URL}/auth/profile`, {
      method: 'PUT',
      headers: getAuthHeader(),
      body: JSON.stringify(data),
    });
    const out = await handleResponse(res);
    // Changing your password ends every session issued against the old one,
    // this tab's included. Core hands back a replacement so the person who
    // just changed it stays signed in; everyone ELSE holding a session for
    // this account is signed out, which is the whole point. Both keys, because
    // login writes both.
    // Nothing to store: Core replaced the session cookie on this same response,
    // so this tab keeps working and every other session for the account is
    // ended by the password fingerprint. Which is the whole point.
    return out;
  } catch (err) {
    return handleError(err);
  }
};

// Signing out is now a SERVER call: the session cookie is HttpOnly, so the panel
// cannot delete what it cannot read. The local hint is dropped first so the UI
// switches immediately rather than waiting on the round trip.
//
// Best-effort on the network. A failed logout leaves a cookie the user cannot
// see and did not want, which is bad - but blocking the sign-out on it would
// leave them staring at a signed-in panel they asked to leave, which is worse.
// The next request 401s and clears it.
export const logout = async (): Promise<void> => {
  forgetSessionHint();
  purgeLegacyTokens();
  try {
    await fetch(`${API_URL}/auth/logout`, { method: 'POST' });
  } catch {
    // See above.
  }
};
// Read-only demo session, no credentials. The account is forced GET-only
// server-side (AuthMiddleware), so this session can look but never change
// anything.
//
// Same storage as a normal login on purpose: from the panel's point of view a
// demo session IS a session, and giving it a second storage path would mean
// every reader had to know about both.
export const demoLogin = async (): Promise<{ success: boolean; username?: string; message?: string }> => {
  try {
    const res = await fetch(`${API_URL}/auth/demo-login`, { method: 'POST' });
    const data: any = await res.json().catch(() => null);
    if (res.ok && data?.token) {
      // Cookie again; nothing stored. A demo session IS a session as far as the
      // panel is concerned, and giving it a second path would mean every reader
      // had to know about both.
      return { success: true, username: data.username };
    }
    return { success: false, message: data?.message || 'No demo account is available.' };
  } catch {
    return { success: false, message: 'Could not reach the server.' };
  }
};

// Whether a demo account exists at all. GET on the same path, minting nothing,
// so a caller can decide whether to OFFER the demo without creating a session
// just to find out.
export const getDemoStatus = async (): Promise<boolean> => {
  try {
    const res = await fetch(`${API_URL}/auth/demo-login`);
    if (!res.ok) return false;
    const data: any = await res.json().catch(() => null);
    return !!data?.available;
  } catch {
    return false;
  }
};
