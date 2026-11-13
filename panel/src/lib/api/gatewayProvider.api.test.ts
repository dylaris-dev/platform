import { describe, it, expect, vi, afterEach } from 'vitest';
import {
    providerSaveBody, providerTestBody, saveGatewayProvider, getGatewayProvider, type ProviderForm,
} from './gatewayProvider';

const form: ProviderForm = {
    mode: 'remote', url: 'https://panel.dylaris.com', key: '', savedUrl: 'https://panel.dylaris.com',
    urlFromEnv: false, keyFromEnv: false,
};

// The rule this decides: what a save sends. Core keeps an OMITTED key or URL
// and clears one sent as "", so sending the empty field would wipe the key on
// every mode switch.
describe('providerSaveBody', () => {
    it.each<[string, Partial<ProviderForm>, object]>([
        ['an empty key and an unchanged URL send only the mode', {}, { mode: 'remote' }],
        ['a typed key is sent, trimmed', { key: '  dyl_abc ' }, { mode: 'remote', key: 'dyl_abc' }],
        ['a trailing slash is not a change', { url: 'https://panel.dylaris.com/' }, { mode: 'remote' }],
        ['a changed URL is sent', { url: 'https://gw.example.com' }, { mode: 'remote', url: 'https://gw.example.com' }],
        ['an emptied URL is sent as "" (back to the default)', { url: '  ' }, { mode: 'remote', url: '' }],
        ['an env URL is never sent', { url: 'https://other.example.com', urlFromEnv: true }, { mode: 'remote' }],
        ['an env key is never sent', { key: 'dyl_x', keyFromEnv: true }, { mode: 'remote' }],
        ['switching to local sends the mode', { mode: 'local' }, { mode: 'local' }],
        // The URL and key fields are hidden in local mode; their content is not sent.
        ['local sends neither a typed key nor a changed URL', { mode: 'local', key: 'dyl_x', url: 'https://x.example.com' }, { mode: 'local' }],
    ])('%s', (_name, patch, want) => {
        expect(providerSaveBody({ ...form, ...patch })).toEqual(want);
    });
});

describe('providerTestBody', () => {
    it('never carries a mode', () => {
        expect(providerTestBody({ ...form, mode: 'local', key: 'k', url: 'https://x.example.com' }))
            .toEqual({ key: 'k', url: 'https://x.example.com' });
        expect(providerTestBody(form)).toEqual({});
    });
});

describe('request shape and refusals', () => {
    afterEach(() => vi.unstubAllGlobals());

    function stub(status: number, body: unknown) {
        const f = vi.fn(async () => new Response(JSON.stringify(body), { status }));
        vi.stubGlobal('fetch', f);
        return f;
    }

    it('POSTs the body as JSON to the settings route', async () => {
        const f = stub(200, { success: true, mode: 'remote', url: 'u', keySet: true, keyFromEnv: false, urlFromEnv: false });
        const res = await saveGatewayProvider({ mode: 'remote' });
        expect(res.success).toBe(true);
        const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
        expect(url).toMatch(/\/settings\/gateway\/provider$/);
        expect(init.method).toBe('POST');
        expect(JSON.parse(init.body as string)).toEqual({ mode: 'remote' });
        expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json');
    });

    it('keeps the code and the server message of a refusal', async () => {
        stub(400, { success: false, message: 'Enter the key again to change the provider address.', code: 'no_key' });
        const res = await saveGatewayProvider({ mode: 'remote', url: 'https://x.example.com' });
        expect(res).toMatchObject({ success: false, status: 400, code: 'no_key', message: 'Enter the key again to change the provider address.' });
    });

    it('reads the gate shape ({error, code})', async () => {
        stub(409, { success: false, error: 'The managed gateway is not enabled.', code: 'gateway_local' });
        const res = await getGatewayProvider();
        expect(res).toMatchObject({ success: false, code: 'gateway_local', message: 'The managed gateway is not enabled.' });
    });

    it('a 429 on save reads as too many attempts', async () => {
        stub(429, { success: false, message: 'Too many requests, try again in a minute.', code: 'rate_limited' });
        const res = await saveGatewayProvider({ mode: 'local' });
        expect(res).toMatchObject({ success: false, status: 429, message: 'Too many attempts, try again in a minute.' });
    });

    it('a transport failure is not an answer', async () => {
        vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('network'); }));
        const res = await getGatewayProvider();
        expect(res).toEqual({ success: false, message: 'Connection failed' });
    });
});
