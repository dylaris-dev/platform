import { afterEach, describe, expect, it, vi } from 'vitest';

import { getGatewayPublicTargets, setGatewayPublicTargets } from './gatewayPublicTargets';

function respond(status: number, body: unknown) {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
    })));
}

afterEach(() => {
    vi.unstubAllGlobals();
});

describe('gateway public targets toggle', () => {
    it('reads the current value', async () => {
        respond(200, { success: true, enabled: true });
        await expect(getGatewayPublicTargets('u1')).resolves.toBe(true);
    });

    it('a failed read raises instead of reporting "off"', async () => {
        respond(403, { success: false, message: 'Forbidden' });
        await expect(getGatewayPublicTargets('u1')).rejects.toThrow();
    });

    it('sends the PUT body and returns the stored value', async () => {
        respond(200, { success: true, enabled: false });
        await expect(setGatewayPublicTargets('u1', false)).resolves.toBe(false);
        const [url, init] = (fetch as unknown as ReturnType<typeof vi.fn>).mock.calls[0];
        expect(String(url)).toContain('/admin/users/u1/gateway-public-targets');
        expect(init.method).toBe('PUT');
        expect(init.body).toBe('{"enabled":false}');
    });

    it('a failed save raises', async () => {
        respond(500, { success: false, message: 'Failed to save the setting' });
        await expect(setGatewayPublicTargets('u1', true)).rejects.toThrow('Failed to save');
    });
});
