import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

type SessionModule = typeof import('./session');

// The module keeps whether this page had a session and whether it is already
// leaving, so every test gets a fresh copy.
let session: SessionModule;
let posted: string[];
let listeners: ((e: { data: unknown }) => void)[];

class FakeChannel {
    onmessage: ((e: { data: unknown }) => void) | null = null;
    constructor() {
        listeners.push((e) => this.onmessage?.(e));
    }
    postMessage(m: string) { posted.push(m); }
    close() {}
}

function page(cookie: string) {
    const loc = { href: '/servers', pathname: '/servers', search: '' };
    vi.stubGlobal('window', { location: loc });
    vi.stubGlobal('document', { cookie });
    vi.stubGlobal('sessionStorage', { setItem: () => {} });
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(null))));
    vi.stubGlobal('BroadcastChannel', FakeChannel);
    return loc;
}

// A live session: the readable hint cookie Core sets beside the real one.
const signedIn = () => page('dylaris_signed_in=1');

beforeEach(async () => {
    posted = [];
    listeners = [];
    vi.resetModules();
    session = await import('./session');
});
afterEach(() => vi.unstubAllGlobals());

describe('what a 401 from a live session does', () => {
    it('signs out an expired session', () => {
        const loc = signedIn();
        expect(session.handleUnauthorized(new Response(null, { status: 401 }))).toBe(true);
        expect(loc.href).toBe('/login');
    });

    // A mistyped password when creating an API key or confirming an admin
    // action is a 401 from a session that is fine. It used to sign the user
    // out and lose their page; Core marks it, and the caller shows the error.
    it('leaves a refused re-authentication to the caller', () => {
        const loc = signedIn();
        const res = new Response(null, { status: 401, headers: { 'X-Dylaris-Reauth': 'failed' } });
        expect(session.handleUnauthorized(res)).toBe(false);
        expect(loc.href).toBe('/servers');
    });

    // The hint cookie expires with the session and Core clears it on the 401
    // that reports the session gone, so by then it is missing. The panel used
    // to read that as "never signed in" and sat there failing every call.
    it('signs out a page that had a session even when the hint is already gone', () => {
        const loc = page('');
        session.markSessionSeen();
        expect(session.handleUnauthorized(new Response(null, { status: 401 }))).toBe(true);
        expect(loc.href).toBe('/login');
    });

    it('leaves a 401 on a page that never had a session to the caller', () => {
        const loc = page('');
        expect(session.handleUnauthorized(new Response(null, { status: 401 }))).toBe(false);
        expect(loc.href).toBe('/servers');
    });

    it('does not treat a missing permission as a sign-out', () => {
        const loc = signedIn();
        expect(session.handleUnauthorized(new Response(null, { status: 403 }))).toBe(false);
        expect(loc.href).toBe('/servers');
    });

    it('leaves once however many calls fail, and tells the other tabs', () => {
        signedIn();
        for (let i = 0; i < 5; i++) session.handleUnauthorized(new Response(null, { status: 401 }));
        expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);
        expect(posted).toEqual(['signed-out']);
    });
});

describe('other tabs', () => {
    it('follow a sign-out to the login page', () => {
        const loc = signedIn();
        session.followSignOuts();
        listeners.forEach((l) => l({ data: 'signed-out' }));
        expect(loc.href).toBe('/login');
    });

    // After its own sign-out the panel moves to /login without a reload; the
    // next login on that page must be able to reach /login again on a 401.
    it('a new session after a sign-out is watched again', () => {
        const loc = signedIn();
        session.announceSignedOut();
        session.markSessionSeen();
        expect(session.handleUnauthorized(new Response(null, { status: 401 }))).toBe(true);
        expect(loc.href).toBe('/login');
    });
});

describe('a cancelled redirect', () => {
    it('does not leave the tab deaf to the next 401', () => {
        vi.useFakeTimers();
        try {
            const loc = signedIn();
            session.handleUnauthorized(new Response(null, { status: 401 }));
            loc.href = '/servers'; // the user stayed on the page
            vi.advanceTimersByTime(5000);
            session.handleUnauthorized(new Response(null, { status: 401 }));
            expect(loc.href).toBe('/login');
        } finally {
            vi.useRealTimers();
        }
    });
});
