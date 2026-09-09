import { isWails } from '@/lib/adapters';

/**
 * Getting somebody to their destination once a sign-in has established a
 * session.
 *
 * Inside the Beam app this cannot be a client-side push, and that is the whole
 * reason this file exists. Beam proxies the panel, and the panel's readable
 * session cookie is replayed into `document.cookie` by a script the proxy
 * splices into HTML DOCUMENTS - because Beam's responses reach WebView2 as
 * custom responses to intercepted requests, and whether that path feeds the
 * browser's own cookie store is undocumented. A `router.push` fetches no
 * document, so on the one transition that needs it the replay never runs: the
 * authed layout finds no session, concludes it is signed out, and pushes
 * straight back to /login. That bounce is what "the app just reloads when I log
 * in" is.
 *
 * A real navigation costs one page load at the single moment a person expects
 * one anyway, and it re-runs every injection the proxy performs - the cookie
 * replay and the settings launcher both.
 *
 * In a browser nothing about this is true, so the push stays: it is faster and
 * it keeps the router's history behaving.
 */
export function navigateAfterLogin(target: string, push: (target: string) => void): void {
    if (isWails()) {
        window.location.href = target;
        return;
    }
    push(target);
}

/**
 * popLoginRedirect returns where to go after signing in, consuming the stash.
 *
 * Consumed rather than read: it is set by whichever page bounced the visitor to
 * the sign-in form, and leaving it behind would send the NEXT sign-in to a page
 * nobody asked for.
 */
export function popLoginRedirect(fallback = '/servers'): string {
    try {
        const target = sessionStorage.getItem('postLoginRedirect');
        sessionStorage.removeItem('postLoginRedirect');
        return target || fallback;
    } catch {
        // Private mode, or storage disabled. The fallback is a real
        // destination, so there is nothing to report here.
        return fallback;
    }
}
