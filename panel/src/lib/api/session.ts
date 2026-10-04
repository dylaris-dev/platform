import { API_URL } from "@/lib/api/core";
import { forgetSessionHint, hasSession } from "@/lib/api/sessionState";

// Shared handling for an expired session. Both API paths (the core.ts
// handleResponse helpers and the legacy fetchAPI wrapper in types.ts) funnel
// their 401 responses through here so the behavior is identical.
//
// A 401 while a token is present means the session expired mid-use. We clear
// both token keys and hard-navigate to /login. The hard navigation (not
// router.push) is deliberate: unloading the document tears down every open
// setInterval poller and EventSource/SSE stream on the page, so none keep
// hammering the API with a dead token. A 401 WITHOUT a token is a failed
// login, not an expired session, so we leave it to the caller.
export function handleUnauthorized(response: Response): boolean {
  if (response.status !== 401 || typeof window === "undefined") return false;

  // A refused RE-AUTHENTICATION (wrong password or code when creating an API
  // key, saving security questions, an admin action) is a 401 from a live
  // session. Core marks it; signing out over a typo lost the user's page.
  if (response.headers.get("X-Dylaris-Reauth") === "failed") return false;

  // A 401 while a session EXISTED means it expired mid-use; a 401 with none is
  // a failed login, which the caller handles. The hint cookie is what tells the
  // two apart now - the token it used to check is not readable any more, and
  // that is the point.
  if (!hasSession()) return false;

  // Optimistic only. The real cookie is HttpOnly and only Core can drop it;
  // this stops the panel rendering an authenticated shell for the moment before
  // the navigation. The server-side clear rides along with the redirect below.
  forgetSessionHint();
  void fetch(`${API_URL}/auth/logout`, { method: 'POST' }).catch(() => {
    // Best-effort: the session is already expired, so there is nothing left to
    // protect and nothing useful to tell the user about a failed cleanup.
  });
  try {
    // Return the user where they were after re-login (login page reads this).
    sessionStorage.setItem("postLoginRedirect", window.location.pathname + window.location.search);
  } catch {
    // sessionStorage can throw in sandboxed/private contexts; the redirect below still works.
  }
  window.location.href = "/login";
  return true;
}
