import { afterEach, describe, expect, it, vi } from 'vitest';

import {
    listCustomDomains,
    issueCustomDomainToken,
    verifyCustomDomainTXT,
    addCustomDomain,
    removeCustomDomain,
    refusalText,
} from './customDomains';

// Core answers a failed request with a JSON envelope and a status. handleResponse
// turns that into a RESOLVED { success: false, message } - deliberately, so a
// caller can render the message instead of "Connection failed".
function respond(status: number, body: unknown) {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
    })));
}

afterEach(() => {
    vi.unstubAllGlobals();
});

// Every one of these used to flatten a failure into a value that reads as a
// perfectly ordinary state, and the three states they flattened to are the three
// answers a customer bringing their own domain most needs to trust:
//
//   list    -> []         "you have no custom domains"
//   token   -> undefined  the unblock button does nothing, silently
//   verify  -> resolves   the check succeeded
//
// The list one is the worst, because CustomDomainsPanel renders NOTHING for an
// empty list. A 403 or a 502 removed the whole section, and with it the TXT
// record that is the only way back from a permanent block.
describe('a failed custom-domain request is not a state', () => {
    it('listCustomDomains raises rather than reporting no domains', async () => {
        respond(500, { success: false, message: 'Could not load your custom domains' });
        await expect(listCustomDomains()).rejects.toThrow('Could not load your custom domains');
    });

    it('listCustomDomains still returns the domains on success', async () => {
        respond(200, { success: true, domains: [{ domain: 'a.example', state: 'pending', attempts: 0 }] });
        await expect(listCustomDomains()).resolves.toEqual({ domains: [{ domain: 'a.example', state: 'pending', attempts: 0 }] });
    });

    it('a success carrying no domains is an empty list, not an error', async () => {
        respond(200, { success: true });
        await expect(listCustomDomains()).resolves.toEqual({ domains: [] });
    });

    it('issueCustomDomainToken raises rather than returning undefined', async () => {
        respond(404, { success: false, message: 'No claim on that domain for your account' });
        await expect(issueCustomDomainToken('a.example')).rejects.toThrow('No claim on that domain');
    });

    // 409 is the case that actually happens: the record is published but has not
    // propagated. It has to reach the customer as "not yet", never as a tick.
    it('verifyCustomDomainTXT raises on the 409 that means "not visible yet"', async () => {
        respond(409, {
            success: false,
            message: 'The TXT record was not found yet. DNS changes can take a few minutes to publish.',
        });
        await expect(verifyCustomDomainTXT('a.example')).rejects.toThrow('not found yet');
    });

    it('verifyCustomDomainTXT returns the message when it really did verify', async () => {
        respond(200, { success: true, message: 'Ownership verified. You can add routes on a.example again.' });
        await expect(verifyCustomDomainTXT('a.example')).resolves.toEqual({
            ok: true, message: 'Ownership verified. You can add routes on a.example again.', providerCooldown: false,
        });
    });

    // A proxy in front of Core answers with HTML, which parses as nothing. That
    // path has no message to carry, and it must still not read as success.
    it('a non-JSON failure body still raises', async () => {
        vi.stubGlobal('fetch', vi.fn(async () => new Response('<html>502</html>', { status: 502 })));
        await expect(listCustomDomains()).rejects.toThrow('Request failed (502)');
    });
});

// The managed gateway: one more proof per claim, and the platform-wide cap.
describe('custom domains on the managed gateway', () => {
    const local = { domain: 'play.own.net', state: 'verified', attempts: 0, txtName: '_dylaris-verify.play.own.net', txtValue: 'dylaris-verify=l' };

    it('sends the same requests as before', async () => {
        const f = vi.fn(async (..._args: unknown[]) => new Response(JSON.stringify({ success: true, domain: local }), { status: 200 }));
        vi.stubGlobal('fetch', f);
        await addCustomDomain('play.own.net');
        await removeCustomDomain('play.own.net');
        await verifyCustomDomainTXT('play.own.net');
        const calls = f.mock.calls.map(c => {
            const init = c[1] as RequestInit | undefined;
            return [String(c[0]).replace(/^.*?\/gateway/, '/gateway'), init?.method, init?.body];
        });
        expect(calls).toEqual([
            ['/gateway/custom-domains', 'POST', JSON.stringify({ domain: 'play.own.net' })],
            ['/gateway/custom-domains/play.own.net', 'DELETE', undefined],
            ['/gateway/custom-domains/play.own.net/verify-txt', 'POST', undefined],
        ]);
    });

    it('carries the provider claim, the limit and a provider error', async () => {
        const provider = { state: 'pending', txtName: '_dylaris-verify.play.own.net', txtValue: 'dylaris-verify=p' };
        respond(200, { success: true, domains: [{ ...local, provider }], limit: { maxCustomDomains: 3, used: 1 } });
        await expect(listCustomDomains()).resolves.toEqual({ domains: [{ ...local, provider }], limit: { maxCustomDomains: 3, used: 1 } });

        respond(200, { success: true, domains: [{ ...local, provider: null }], providerError: { code: 'provider_unreachable', message: 'x' } });
        const down = await listCustomDomains();
        expect(down.domains[0].provider).toBeNull();
        expect(down.providerError).toEqual({ code: 'provider_unreachable', message: 'x' });
        expect(down.limit).toBeUndefined();
    });

    // 409 here is "neither side advanced", and it still reports both sides.
    it('a 409 that reports both sides is an answer, not a throw', async () => {
        respond(409, { success: false, local: 'pending', provider: 'pending', message: 'Neither TXT record was found yet.' });
        await expect(verifyCustomDomainTXT('play.own.net')).resolves.toEqual({
            ok: false, local: 'pending', provider: 'pending', message: 'Neither TXT record was found yet.', providerCooldown: false,
        });
    });

    // Only Core's flag counts; the sentence alone is not read.
    it('reads the provider cooldown from its flag, and a provider error', async () => {
        const message = 'This platform has verified play.own.net. The gateway provider checked it a moment ago: try again in a few minutes.';
        respond(200, { success: true, local: 'verified', provider: 'pending', providerCooldown: true, message });
        await expect(verifyCustomDomainTXT('play.own.net')).resolves.toMatchObject({ ok: true, local: 'verified', provider: 'pending', providerCooldown: true });
        respond(200, { success: true, local: 'verified', provider: 'pending', message });
        await expect(verifyCustomDomainTXT('play.own.net')).resolves.toMatchObject({ providerCooldown: false });

        respond(200, { success: true, local: 'verified', provider: 'pending', message: 'm', providerError: { code: 'provider_unreachable', message: 'down' } });
        await expect(verifyCustomDomainTXT('play.own.net')).resolves.toMatchObject({ providerCooldown: false, providerError: { code: 'provider_unreachable' } });
    });

    // The gateway error shape puts its text in `error`; handleResponse alone
    // would have said "Unknown error".
    it('a refusal shows the server text, else the sentence for its code', async () => {
        respond(409, { success: false, error: 'Own domains are not enabled for this account on the managed gateway.', code: 'custom_domains_off' });
        await expect(addCustomDomain('play.own.net')).rejects.toMatchObject({
            message: 'Own domains are not enabled for this account on the managed gateway.', code: 'custom_domains_off',
        });
        respond(409, { success: false, code: 'custom_domain_limit' });
        await expect(addCustomDomain('play.own.net')).rejects.toThrow("This platform's own-domain limit at the gateway provider is reached. Ask the platform operator.");
        respond(409, { success: false, code: 'brand_new_code' });
        await expect(removeCustomDomain('play.own.net')).rejects.toThrow('Could not remove the domain.');
    });
});

// Two refusals exist in two shapes each (task-10-report "Known limits"): the
// local one in the session shape (`message`), the provider's in the gateway
// shape (`error`). Each keeps its own text.
describe('one code, two shapes', () => {
    it.each([
        ['too_many_domains, local cap', { success: false, message: 'You can add up to 50 domains.', code: 'too_many_domains' }, 'You can add up to 50 domains.'],
        ['too_many_domains, provider cap', { success: false, error: "This platform's own-domain limit at the gateway provider is reached.", code: 'too_many_domains' },
            "This platform's own-domain limit at the gateway provider is reached."],
        ['custom_domains_off, local switch', { success: false, message: 'Your own domains are not enabled on this platform.', code: 'custom_domains_off' },
            'Your own domains are not enabled on this platform.'],
        ['custom_domains_off, provider switch', { success: false, error: 'Own domains are not enabled for this account on the managed gateway.', code: 'custom_domains_off' },
            'Own domains are not enabled for this account on the managed gateway.'],
        ['pending_domains_limit', { success: false, message: 'Finish verifying your pending domains before adding another.', code: 'pending_domains_limit' },
            'Finish verifying your pending domains before adding another.'],
    ])('%s', async (_n, body, want) => {
        respond(409, body);
        await expect(addCustomDomain('play.own.net')).rejects.toMatchObject({ message: want, code: body.code });
    });

    it('pending_domains_limit without text still says what to do', async () => {
        respond(409, { success: false, code: 'pending_domains_limit' });
        await expect(addCustomDomain('play.own.net')).rejects.toThrow('Finish verifying your pending domains before adding another.');
    });
});

describe('a 429 on Add or Verify', () => {
    // The platform-wide own-domain limiter names itself; its text wins.
    it("shows the server's text when it sends one", async () => {
        respond(429, { success: false, message: 'Too many own-domain changes on this platform right now. Try again in a minute.', code: 'rate_limited' });
        await expect(addCustomDomain('play.own.net')).rejects.toThrow('Too many own-domain changes on this platform right now. Try again in a minute.');
        respond(429, { success: false, error: 'Too many own-domain changes on this platform right now. Try again in a minute.', code: 'rate_limited' });
        await expect(verifyCustomDomainTXT('play.own.net')).rejects.toThrow('Too many own-domain changes on this platform right now.');
    });

    it('is "too many attempts" without text', async () => {
        respond(429, { success: false, code: 'rate_limited' });
        await expect(addCustomDomain('play.own.net')).rejects.toThrow('Too many attempts, try again in a minute.');
        respond(429, { success: false });
        await expect(verifyCustomDomainTXT('play.own.net')).rejects.toThrow('Too many attempts, try again in a minute.');
    });

    it('custom_domain_cap on Add keeps its text, or maps without one', async () => {
        respond(403, { success: false, message: 'Your plan on this platform allows no own domains.', code: 'custom_domain_cap' });
        await expect(addCustomDomain('play.own.net')).rejects.toMatchObject({ message: 'Your plan on this platform allows no own domains.', code: 'custom_domain_cap' });
        respond(403, { success: false, code: 'custom_domain_cap' });
        await expect(addCustomDomain('play.own.net')).rejects.toThrow('You have reached your limit of routes on your own domains.');
    });

    // The provider error now also rides on the verify answer.
    it('a verify answer carries providerError on both statuses', async () => {
        respond(409, { success: false, local: 'pending', provider: 'pending', message: 'm', providerError: { code: 'provider_unreachable', message: 'down' } });
        await expect(verifyCustomDomainTXT('play.own.net')).resolves.toMatchObject({ ok: false, providerError: { code: 'provider_unreachable', message: 'down' } });
    });

    // The verify cooldown is a 429 too, and its text says how long to wait.
    it('keeps the cooldown text', async () => {
        respond(429, { success: false, message: 'You checked this domain a moment ago. Try again in 4 minutes.', code: 'verify_cooldown' });
        await expect(verifyCustomDomainTXT('play.own.net')).rejects.toThrow('Try again in 4 minutes.');
    });
});

describe('refusalText', () => {
    it.each([
        [{ message: 'm', error: 'e', code: 'domain_in_use' }, 'm'],
        [{ error: 'e', code: 'domain_in_use' }, 'e'],
        [{ code: 'domain_in_use' }, 'Delete the routes on this domain first.'],
        [{ code: 'custom_domain_cap' }, 'You have reached your limit of routes on your own domains.'],
        [{ code: 'unknown_code' }, 'fallback'],
        [{}, 'fallback'],
        [null, 'fallback'],
    ])('%j -> %s', (body, want) => {
        expect(refusalText(body, 'fallback')).toBe(want);
    });
});
