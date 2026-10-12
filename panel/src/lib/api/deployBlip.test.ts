import { afterEach, describe, expect, it, vi } from 'vitest';

import { getProfile } from './auth';
import { getSetupStatus } from './setup';

// During a deploy the proxy answers 502 with an HTML page for a moment. The
// panel read that as "signed out" (profile null) and as "setup not complete"
// (no mode), and sent open tabs to /login or to the setup wizard.
function answer(status: number, body = '<html>Bad Gateway</html>') {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response(body, { status }))));
}

afterEach(() => vi.unstubAllGlobals());

describe('a deploy is not a sign-out', () => {
    it('a 502 on the profile is "could not ask", not "signed out"', async () => {
        answer(502);
        await expect(getProfile()).rejects.toThrow();
    });

    it('only a 401 on the profile means signed out', async () => {
        answer(401, '{"message":"Invalid Token"}');
        await expect(getProfile()).resolves.toBeNull();
    });

    it('a 502 on the setup status does not send anyone to the wizard', async () => {
        answer(502);
        expect((await getSetupStatus()).mode).toBe('complete');
    });
});
