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

    // A customer's machine used to be excluded, because its node always ran a
    // Link inside itself and a zero could hardly happen. The node starts none any
    // more, so zero on a BYON machine is what a customer gets by updating their
    // node image without redeploying their file - the likeliest way this breaks,
    // and the one the warning has to cover.
    it('warns for any machine with no Link, customer machines included', () => {
        expect(linkMissing(true, { linkCount: 0, tags: 'external' })).toBe(true);
        expect(linkMissing(true, { linkCount: 0, ownerId: 'tenant-1' })).toBe(true);
    });
});
