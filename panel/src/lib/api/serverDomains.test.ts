import { describe, it, expect, vi, afterEach } from 'vitest';
import { updateDnsAccount, createDnsAccount, buildCredentialBody } from './serverDomains';

afterEach(() => vi.unstubAllGlobals());

// A blank secret field on an edit means "keep the stored value". Core merges
// only non-empty fields, but the panel must not send blanks at all: a client
// that sent '' would rely on every future Core version treating it as "keep".
describe('buildCredentialBody', () => {
    it('omits blank and whitespace-only fields', () => {
        expect(buildCredentialBody({ token: '', zone: '  ', email: 'a@b.c' })).toEqual({ email: 'a@b.c' });
    });
    it('keeps a typed value untouched, including inner spaces', () => {
        expect(buildCredentialBody({ token: 'ab cd' })).toEqual({ token: 'ab cd' });
    });
});

describe('updateDnsAccount', () => {
    it('does not send a blank secret field (keeps the stored value)', async () => {
        const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ success: true }), { status: 200 }));
        vi.stubGlobal('fetch', fetchMock);
        await updateDnsAccount(7, { name: 'main', provider: 'cloudflare', credentials: { apiToken: '', email: 'x@y.z' } });
        const [url, init] = fetchMock.mock.calls[0];
        expect(String(url)).toContain('/admin/dns/accounts/7');
        expect(init.method).toBe('PUT');
        expect(JSON.parse(init.body)).toEqual({ name: 'main', provider: 'cloudflare', credentials: { email: 'x@y.z' } });
    });
});

describe('createDnsAccount', () => {
    it('posts the credentials as typed', async () => {
        const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ success: true }), { status: 200 }));
        vi.stubGlobal('fetch', fetchMock);
        await createDnsAccount({ name: 'main', provider: 'desec', credentials: { token: 't' } });
        const [, init] = fetchMock.mock.calls[0];
        expect(init.method).toBe('POST');
        expect(JSON.parse(init.body).credentials).toEqual({ token: 't' });
    });
});
