import { describe, it, expect } from 'vitest';
import { serverOption, nodeOption, routeOption, hasAnyCreateOption, type CreateOptionsInput } from './createOptions';

const base: CreateOptionsInput = {
    isAdmin: false,
    byonEnabled: false,
    entitledByon: false,
    deployableNodes: 0,
    storeEnabled: false,
    gatewayEnabled: false,
    routeOnlyEnabled: true,
    entitledRouteOnly: false,
};
const w = (o: Partial<CreateOptionsInput>): CreateOptionsInput => ({ ...base, ...o });

describe('serverOption', () => {
    it('lets an admin through', () => {
        expect(serverOption(w({ isAdmin: true, byonEnabled: true })).enabled).toBe(true);
    });

    // The self-host case. POST /api/servers has no capability gate, so a user
    // really can create here; hiding the button would misrepresent the backend.
    it('lets any user through when BYON is off', () => {
        expect(serverOption(w({})).enabled).toBe(true);
    });

    it('lets a tenant through once they own a deployable node', () => {
        expect(serverOption(w({ byonEnabled: true, entitledByon: true, deployableNodes: 1 })).enabled).toBe(true);
    });

    it('points an entitled tenant with no node at adding one', () => {
        const o = serverOption(w({ byonEnabled: true, entitledByon: true }));
        expect(o.enabled).toBe(false);
        expect(o.reason).toMatch(/node first/i);
        expect(o.href).toBe('/nodes');
    });

    it('explains rather than just greying out for an unentitled tenant', () => {
        const o = serverOption(w({ byonEnabled: true }));
        expect(o.enabled).toBe(false);
        expect(o.reason).toBeTruthy();
    });
});

describe('nodeOption', () => {
    it('is unavailable and says so when BYON is off', () => {
        const o = nodeOption(w({}));
        expect(o.enabled).toBe(false);
        expect(o.reason).toMatch(/turned off/i);
    });

    // An admin gets no href: adding a node happens on the host, so the caller
    // opens the instructions modal instead of navigating. The old href pointed at
    // /settings?tab=warp, a query shape the settings routes never read, and it
    // silently landed on whatever tab came first.
    it('offers an admin the node flow with no destination to navigate to', () => {
        const o = nodeOption(w({ byonEnabled: true, isAdmin: true }));
        expect(o.enabled).toBe(true);
        expect(o.href).toBeUndefined();
    });

    // Adding a node is an operator action on every install. Gating it on the
    // tenant BYON flag hid it on exactly the self-host installs where the admin is
    // the only one who can add nodes at all.
    it('stays available to an admin with BYON off', () => {
        expect(nodeOption(w({ byonEnabled: false, isAdmin: true })).enabled).toBe(true);
    });

    it('is available to an entitled tenant', () => {
        expect(nodeOption(w({ byonEnabled: true, entitledByon: true })).enabled).toBe(true);
    });

    // With no storefront configured there is nothing to link to, so the copy has
    // to name a human instead of dangling a dead "get it" link.
    it('tells an unentitled tenant to ask an admin when there is no store', () => {
        const o = nodeOption(w({ byonEnabled: true }));
        expect(o.enabled).toBe(false);
        expect(o.reason).toMatch(/ask an admin/i);
        expect(o.href).toBeUndefined();
    });

    it('offers a link when a store is configured', () => {
        const o = nodeOption(w({ byonEnabled: true, storeEnabled: true }));
        expect(o.enabled).toBe(false);
        expect(o.href).toBeTruthy();
        expect(o.hrefLabel).toBeTruthy();
    });
});

describe('hasAnyCreateOption', () => {
    it('is true for a plain self-host user (servers work)', () => {
        expect(hasAnyCreateOption(w({}))).toBe(true);
    });

    it('is false for a tenant with no entitlement and no node', () => {
        expect(hasAnyCreateOption(w({ byonEnabled: true }))).toBe(false);
    });

    it('is true as soon as BYON is granted', () => {
        expect(hasAnyCreateOption(w({ byonEnabled: true, entitledByon: true }))).toBe(true);
    });
});

describe('routeOption', () => {
    // Route-only is a product of its own - the customer runs the server, we
    // give it an address - and it had no entry in the create menu at all; its
    // page hung off the account dropdown, where nobody looks for a product.
    it('is offered to an entitled tenant', () => {
        const o = routeOption(w({ gatewayEnabled: true, entitledRouteOnly: true }));
        expect(o.enabled).toBe(true);
        expect(o.href).toBe('/routes');
    });

    it('is offered to an admin without an entitlement of their own', () => {
        expect(routeOption(w({ gatewayEnabled: true, isAdmin: true })).enabled).toBe(true);
    });

    // An admin is NOT exempt from this one: with routing off there is no edge
    // to point an address at, so the entry would be an offer nothing can fill.
    it('is refused for everyone when gateway routing is off', () => {
        expect(routeOption(w({ isAdmin: true })).enabled).toBe(false);
        expect(routeOption(w({ isAdmin: true })).reason).toContain('Gateway routing');
        expect(routeOption(w({ entitledRouteOnly: true })).enabled).toBe(false);
    });

    it('sends an unentitled tenant to the store only when there is one', () => {
        const withStore = routeOption(w({ gatewayEnabled: true, storeEnabled: true }));
        expect(withStore.enabled).toBe(false);
        expect(withStore.href).toBe('/nodes');

        const noStore = routeOption(w({ gatewayEnabled: true }));
        expect(noStore.href).toBeUndefined();
        expect(noStore.reason).toContain('Ask an admin');
    });

    // The "+" hides itself when nothing is on offer; route-only has to count,
    // or a gateway-only install with no BYON would lose the button entirely.
    it('counts towards the menu having something to show', () => {
        expect(hasAnyCreateOption(w({ gatewayEnabled: true, entitledRouteOnly: true, byonEnabled: true }))).toBe(true);
    });
});

// Route-only has its own switch; off, not even an admin is offered an address.
describe('routeOption with route-only switched off', () => {
    it('is refused with a reason', () => {
        const o = routeOption(w({ gatewayEnabled: true, isAdmin: true, routeOnlyEnabled: false }));
        expect(o.enabled).toBe(false);
        expect(o.reason).toMatch(/turned off/);
    });
});
