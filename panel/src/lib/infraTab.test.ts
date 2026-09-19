import { describe, it, expect } from 'vitest';
import { resolveInfraTab, showInfraTabBar, infraAvailability } from './infraTab';

// An operator: all three halves.
const admin = { external: true, machines: true, routes: true };
// A tenant on a full platform: no external tab.
const tenant = { external: false, machines: true, routes: true };
const machinesOnly = { external: false, machines: true, routes: false };
const routesOnly = { external: false, machines: false, routes: true };
const neither = { external: false, machines: false, routes: false };

describe('resolveInfraTab', () => {
    it('defaults an admin to their own machines', () => {
        expect(resolveInfraTab(null, admin)).toBe('external');
        expect(resolveInfraTab('external', admin)).toBe('external');
    });

    it('defaults a tenant to bring-your-own-node', () => {
        expect(resolveInfraTab(null, tenant)).toBe('machines');
        expect(resolveInfraTab('machines', tenant)).toBe('machines');
    });

    // The external tab is the operator's own hardware. Core refuses
    // ?scope=external for a non-admin as well, so this is the visible half of a
    // rule that is enforced on both sides.
    it('sends a tenant who asks for the external tab to their own', () => {
        expect(resolveInfraTab('external', tenant)).toBe('machines');
        expect(resolveInfraTab('external', routesOnly)).toBe('routes');
        expect(resolveInfraTab('external', neither)).toBeNull();
    });

    it('honours an explicit routes request when routes exist', () => {
        expect(resolveInfraTab('routes', admin)).toBe('routes');
        expect(resolveInfraTab('routes', tenant)).toBe('routes');
    });

    // First bug: a bookmarked ?tab=routes opened a panel whose two endpoints
    // 409 without gateway routing. Hiding the tab bar was not enough, because
    // the URL still decided.
    it('ignores a routes request where there is no gateway routing', () => {
        expect(resolveInfraTab('routes', machinesOnly)).toBe('machines');
    });

    // Second bug: the page returned "your own hardware is turned off" on a
    // BYON-off platform, with the routes tab unreachable behind it - while the
    // top bar happily offered the page to route-only customers.
    it('goes straight to routes when there are no machines', () => {
        expect(resolveInfraTab(null, routesOnly)).toBe('routes');
        expect(resolveInfraTab('machines', routesOnly)).toBe('routes');
    });

    // An admin on a platform with BYON off still has their own machines.
    it('keeps the external tab for an admin with BYON off', () => {
        const byonOff = { external: true, machines: false, routes: true };
        expect(resolveInfraTab(null, byonOff)).toBe('external');
        expect(resolveInfraTab('machines', byonOff)).toBe('external');
    });

    it('is null when nothing exists', () => {
        expect(resolveInfraTab(null, neither)).toBeNull();
        expect(resolveInfraTab('routes', neither)).toBeNull();
    });

    it('treats an unknown tab value as the default', () => {
        expect(resolveInfraTab('nonsense', admin)).toBe('external');
        expect(resolveInfraTab('nonsense', tenant)).toBe('machines');
    });
});

describe('showInfraTabBar', () => {
    it('appears only when there is more than one place to go', () => {
        expect(showInfraTabBar(admin)).toBe(true);
        expect(showInfraTabBar(tenant)).toBe(true);
        expect(showInfraTabBar({ external: true, machines: false, routes: false })).toBe(false);
        expect(showInfraTabBar(machinesOnly)).toBe(false);
        expect(showInfraTabBar(routesOnly)).toBe(false);
        expect(showInfraTabBar(neither)).toBe(false);
    });
});

describe('infraAvailability', () => {
    // The regression this pairing exists for: on a gateway-routed install with
    // BYON off, `routes` used to read gatewayEnabled and opened the route-only
    // half, whose every read answers 403 "BYON is not enabled" - to admins too.
    it('closes route-only when BYON is off, even for an admin', () => {
        expect(infraAvailability(true, false)).toEqual({
            external: true, machines: false, routes: false,
        });
    });

    it('opens both tenant halves once BYON is usable', () => {
        expect(infraAvailability(false, true)).toEqual({
            external: false, machines: true, routes: true,
        });
    });

    // external is the operator's own half and is never a feature flag.
    it('keeps external admin-only regardless of BYON', () => {
        expect(infraAvailability(false, true).external).toBe(false);
        expect(infraAvailability(true, false).external).toBe(true);
    });

    // With nothing available the page must be able to say so rather than render
    // an empty tab - resolveInfraTab returns null only when the map is all false.
    it('leaves a non-admin on a BYON-less platform with no tab at all', () => {
        expect(resolveInfraTab(null, infraAvailability(false, false))).toBeNull();
    });

    // Route-only can be on without BYON (and off beside it) now that it has
    // its own switch.
    it('opens route-only on its own flag', () => {
        expect(infraAvailability(false, false, true)).toEqual({ external: false, machines: false, routes: true });
        expect(infraAvailability(false, true, false)).toEqual({ external: false, machines: true, routes: false });
    });

    it('does not strand a reader who asked for a tab they lost', () => {
        expect(resolveInfraTab('routes', infraAvailability(true, false))).toBe('external');
    });
});
