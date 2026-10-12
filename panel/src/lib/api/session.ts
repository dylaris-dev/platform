import { API_URL } from "@/lib/api/core";
import { forgetSessionHint, hasSession } from "@/lib/api/sessionState";

// Shared handling for an expired session. Both API paths (the core.ts
// handleResponse helpers and the legacy fetchAPI wrapper in types.ts) funnel
// their 401 responses through here so the behavior is identical.
//
// A 401 while a session exists means it ended mid-use. We hard-navigate to
// /login. The hard navigation (not router.push) is deliberate: unloading the
// document tears down every open setInterval poller and EventSource/SSE stream
// on the page, so none keep hammering the API with a dead session. A 401 with
// no session behind it is a failed login, not an expired session, so we leave
// it to the caller.

// Set by the authed layout once it has let a page through. The hint cookie
// alone could not tell: it expires together with the session, and Core clears
// it on the very 401 that reports the session gone - so the one moment it was
// needed, it was already missing, and the panel sat there failing every call.
let sessionSeen = false;
let redirecting = false;

// Also re-arms the redirect: after a sign-out the panel moves to /login
// without a reload, so this module outlives it into the next session.
export function markSessionSeen(): void {
  sessionSeen = true;
  redirecting = false;
}

const CHANNEL = "dylaris-auth";

function authChannel(): BroadcastChannel | null {
  if (typeof BroadcastChannel === "undefined") return null;
  try {
    return new BroadcastChannel(CHANNEL);
  } catch {
    return null;
  }
}

// Tells the panel's other tabs that this browser is signed out. They share the
// cookie, so they are signed out too; without this they only noticed on their
// next request, which for an idle tab is never.
export function announceSignedOut(): void {
  // This tab is leaving on its own terms; its own listener must not redirect
  // it a second time with the page it is leaving saved as the way back.
  redirecting = true;
  sessionSeen = false;
  const ch = authChannel();
  if (!ch) return;
  ch.postMessage("signed-out");
  ch.close();
}

// Sends this tab to /login, once, remembering where it was.
export function goToLogin(): void {
  if (redirecting || typeof window === "undefined") return;
  redirecting = true;
  forgetSessionHint();
  try {
    // Return the user where they were after re-login (login page reads this).
    sessionStorage.setItem("postLoginRedirect", window.location.pathname + window.location.search);
  } catch {
    // sessionStorage can throw in sandboxed/private contexts; the redirect below still works.
  }
  window.location.href = "/login";
  // A navigation the user cancels ("leave this page?") leaves this tab here;
  // without the reset every later 401 would be swallowed by the guard above.
  setTimeout(() => {
    redirecting = false;
  }, 5000);
}

// Follows a sign-out announced by another tab. Returns the unsubscribe.
export function followSignOuts(): () => void {
  const ch = authChannel();
  if (!ch) return () => {};
  ch.onmessage = (e) => {
    if (e.data === "signed-out") goToLogin();
  };
  return () => ch.close();
}

export function handleUnauthorized(response: Response): boolean {
  if (response.status !== 401 || typeof window === "undefined") return false;

  // A refused RE-AUTHENTICATION (wrong password or code when creating an API
  // key, saving security questions, an admin action) is a 401 from a live
  // session. Core marks it; signing out over a typo lost the user's page.
  if (response.headers.get("X-Dylaris-Reauth") === "failed") return false;

  // No session was ever in play on this page: a failed login, a 2FA setup or a
  // password reset. The caller shows its own error.
  if (!sessionSeen && !hasSession()) return false;
  if (redirecting) return true;

  // The real cookie is HttpOnly and only Core can drop it; the server-side
  // clear rides along with the redirect.
  void fetch(`${API_URL}/auth/logout`, { method: "POST" }).catch(() => {
    // Best-effort: the session is already over, so there is nothing left to
    // protect and nothing useful to tell the user about a failed cleanup.
  });
  goToLogin();
  announceSignedOut();
  return true;
}
