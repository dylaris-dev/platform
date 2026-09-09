import { describe, it, expect, vi, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { navigateAfterLogin, popLoginRedirect } from './postLogin';

// Signing in inside the Beam app bounced straight back to the sign-in form.
// The panel's readable session cookie is replayed into document.cookie by a
// script the Beam proxy splices into HTML DOCUMENTS; a router.push fetches no
// document, so on that one transition the replay never ran, the authed layout
// found no session and pushed back to /login.
//
// The rule is therefore about HOW a post-login navigation happens, and it holds
// only if every site that establishes a session actually asks. Both halves are
// pinned: the behaviour, and the call sites.

type FakeWindow = { go?: { main?: { App?: object } }; location: { href: string } };

function withWindow(w: FakeWindow | undefined) {
    (globalThis as unknown as { window?: FakeWindow }).window = w;
}

afterEach(() => {
    delete (globalThis as unknown as { window?: FakeWindow }).window;
    delete (globalThis as unknown as { sessionStorage?: Storage }).sessionStorage;
});

describe('navigateAfterLogin', () => {
    it('loads the document when the panel is running inside Beam', () => {
        const win: FakeWindow = { go: { main: { App: {} } }, location: { href: '' } };
        withWindow(win);
        const push = vi.fn();

        navigateAfterLogin('/servers', push);

        expect(win.location.href).toBe('/servers');
        expect(push, 'a client-side push is the thing that skips the cookie replay').not.toHaveBeenCalled();
    });

    it('pushes in an ordinary browser', () => {
        // Nothing about the Beam proxy applies here, and a full load would cost
        // a page render at the moment a person is least willing to wait.
        withWindow({ location: { href: '' } });
        const push = vi.fn();

        navigateAfterLogin('/servers', push);

        expect(push).toHaveBeenCalledWith('/servers');
    });
});

describe('popLoginRedirect', () => {
    function fakeStorage(initial: Record<string, string>) {
        const map = new Map(Object.entries(initial));
        (globalThis as unknown as { sessionStorage: unknown }).sessionStorage = {
            getItem: (k: string) => map.get(k) ?? null,
            removeItem: (k: string) => { map.delete(k); },
        };
        return map;
    }

    it('returns the stashed destination and consumes it', () => {
        // Consumed, or the NEXT sign-in lands somewhere nobody asked for.
        const map = fakeStorage({ postLoginRedirect: '/servers/7/files' });
        expect(popLoginRedirect()).toBe('/servers/7/files');
        expect(map.has('postLoginRedirect')).toBe(false);
    });

    it('falls back when nothing was stashed', () => {
        fakeStorage({});
        expect(popLoginRedirect()).toBe('/servers');
    });

    it('falls back when storage throws', () => {
        // Private mode. A sign-in must still complete.
        (globalThis as unknown as { sessionStorage: unknown }).sessionStorage = {
            getItem() { throw new Error('denied'); },
            removeItem() { throw new Error('denied'); },
        };
        expect(popLoginRedirect()).toBe('/servers');
    });
});

describe('the sign-in sites ask', () => {
    const src = (...parts: string[]) => readFileSync(join(__dirname, '..', ...parts), 'utf8');

    it('the sign-in form navigates through the helper', () => {
        const code = src('components', 'LoginForm.tsx');
        expect(code).toMatch(/navigateAfterLogin\(popLoginRedirect\(\), router\.push\)/);
    });

    it('the demo sign-in does too', () => {
        // It establishes a session exactly like the form does, and was the
        // second copy of this navigation.
        const code = src('app', 'login', 'page.tsx');
        expect(code).toMatch(/navigateAfterLogin\('\/servers', router\.replace\)/);
    });

    it('nothing else reads the stashed redirect by hand', () => {
        // It was read in two places and written in two others. The reads are
        // what carry the rule, so they live in one file - a third copy is how
        // the next sign-in path quietly skips the Beam branch.
        const roots = [
            src('components', 'LoginForm.tsx'),
            src('app', 'login', 'page.tsx'),
            src('app', '(authed)', 'layout.tsx'),
        ];
        for (const code of roots) {
            expect(code).not.toMatch(/getItem\(\s*['"]postLoginRedirect['"]\s*\)/);
        }
    });
});
