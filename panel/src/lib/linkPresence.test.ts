import { describe, expect, it } from 'vitest';
import { linkMissing } from './linkPresence';
import { isGatewayRouting } from './api/types';

describe('linkMissing', () => {
    it('warns for a gateway-routed node whose heartbeat counts no Link', () => {
        expect(linkMissing(isGatewayRouting('gateway'), { linkCount: 0 })).toBe(true);
        expect(linkMissing(isGatewayRouting('both'), { linkCount: 0 })).toBe(true);
    });

    it('is quiet on ip_port, where no player path runs through a Link', () => {
        expect(linkMissing(isGatewayRouting('ip_port'), { linkCount: 0 })).toBe(false);
    });

    it('does not read an unknown count as zero', () => {
        expect(linkMissing(isGatewayRouting('gateway'), {})).toBe(false);
        expect(linkMissing(isGatewayRouting('gateway'), { linkCount: null })).toBe(false);
    });

    it('is quiet when a Link runs', () => {
        expect(linkMissing(isGatewayRouting('gateway'), { linkCount: 1 })).toBe(false);
    });

    it('warns for an operator external node, never for a customer machine', () => {
        expect(linkMissing(true, { linkCount: 0, tags: 'external' })).toBe(true);
        expect(linkMissing(true, { linkCount: 0, ownerId: 'tenant-1' })).toBe(false);
    });
});
