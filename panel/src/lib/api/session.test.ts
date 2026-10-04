import { afterEach, describe, expect, it, vi } from 'vitest';

import { handleUnauthorized } from './session';

// A live session: the readable hint cookie Core sets beside the real one.
function signedIn() {
    const loc = { href: '/servers', pathname: '/servers', search: '' };
    vi.stubGlobal('window', { location: loc });
    vi.stubGlobal('document', { cookie: 'dylaris_signed_in=1' });
    vi.stubGlobal('sessionStorage', { setItem: () => {} });
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(null))));
    return loc;
}

afterEach(() => vi.unstubAllGlobals());

describe('what a 401 from a live session does', () => {
    it('signs out an expired session', () => {
        const loc = signedIn();
        expect(handleUnauthorized(new Response(null, { status: 401 }))).toBe(true);
        expect(loc.href).toBe('/login');
    });

    // A mistyped password when creating an API key or confirming an admin
    // action is a 401 from a session that is fine. It used to sign the user
    // out and lose their page; Core marks it, and the caller shows the error.
    it('leaves a refused re-authentication to the caller', () => {
        const loc = signedIn();
        const res = new Response(null, { status: 401, headers: { 'X-Dylaris-Reauth': 'failed' } });
        expect(handleUnauthorized(res)).toBe(false);
        expect(loc.href).toBe('/servers');
    });
});
