// What the sidebar's "+" may offer, and why an entry is unavailable.
//
// Pure so the reasons are testable: the wording IS the feature here. A greyed
// button with no explanation is the thing this replaces - it used to be an
// always-visible "New Container" that a user without a node could click and
// only then be told "No node available".

export interface CreateOptionsInput {
    isAdmin: boolean;
    /** feature_byon_enabled. With BYON off there is no tenant model at all. */
    byonEnabled: boolean;
    /** The caller may enroll their own nodes (resolved entitlement). */
    entitledByon: boolean;
    /** Nodes the caller could actually deploy a server on. */
    deployableNodes: number;
    /** features.store: a storefront is configured, so "get a plan" can link somewhere. */
    storeEnabled: boolean;
    /** Gateway routing is on. Route-only does not exist without it. */
    gatewayEnabled: boolean;
    /** The route-only switch (feature_route_only_enabled, following BYON while unset). */
    routeOnlyEnabled: boolean;
    /** The caller may create protected addresses (resolved entitlement). */
    entitledRouteOnly: boolean;
}

export interface CreateOption {
    /** false renders the entry greyed out, with `reason` shown underneath. */
    enabled: boolean;
    reason?: string;
    /** Where "learn more"/"get it" should point, when there is somewhere to go. */
    href?: string;
    hrefLabel?: string;
}

/**
 * Whether the caller can create a server, and why not.
 *
 * An admin always can. With BYON off every authed user can, which is the
 * existing self-host behaviour (POST /api/servers has no capability gate) - so
 * refusing here would hide a button that actually works. With BYON on, a tenant
 * can only deploy on their OWN nodes, so no node means no server.
 */
export function serverOption(i: CreateOptionsInput): CreateOption {
    if (i.isAdmin) return { enabled: true };
    if (!i.byonEnabled) return { enabled: true };
    if (i.deployableNodes > 0) return { enabled: true };
    if (i.entitledByon) {
        return {
            enabled: false,
            reason: 'You need a node first. Add your own machine, then deploy servers on it.',
            href: '/nodes',
            hrefLabel: 'Add a node',
        };
    }
    return {
        enabled: false,
        reason: 'Servers run on your own machine. Your account is not set up for that yet.',
        ...(i.storeEnabled ? { href: '/nodes', hrefLabel: 'See how it works' } : {}),
    };
}

/**
 * Whether the caller can add their own node, and why not.
 *
 * An admin always can, and gets the fleet instructions rather than the tenant
 * enrolment page: enrolling a platform node is a different flow from a tenant
 * adding theirs, and conflating them would put an admin through the tenant path
 * by accident.
 */
export function nodeOption(i: CreateOptionsInput): CreateOption {
    // Admins first: adding a node is an operator action that exists on every
    // install, BYON or not. Gating it on the tenant feature flag hid the entry on
    // exactly the self-host installs where the operator is the only one who can
    // add nodes at all. `href` is empty because there is no page to send them to
    // — the caller opens the instructions modal.
    if (i.isAdmin) return { enabled: true };
    if (!i.byonEnabled) {
        return {
            enabled: false,
            reason: 'Bring-your-own-node is turned off on this platform.',
        };
    }
    if (i.entitledByon) return { enabled: true, href: '/nodes' };
    return {
        enabled: false,
        reason: 'Your account does not include bring-your-own-node yet.',
        ...(i.storeEnabled
            ? { href: '/nodes', hrefLabel: 'How to get it' }
            : { reason: 'Your account does not include bring-your-own-node. Ask an admin to enable it.' }),
    };
}

/**
 * Whether the caller can create a protected address, and why not.
 *
 * Route-only is a product in its own right - the customer runs the Minecraft
 * server themselves and Dylaris only gives it an address - so it belongs beside
 * the other two things a tenant can create. It had no entry here at all, and
 * its page hung off the account dropdown, where nobody looks for a product.
 *
 * An admin is not exempt from the gateway check the way they are from the
 * entitlement one: with routing off there is no edge to point an address at, so
 * the entry would be an offer nothing can fulfil.
 */
export function routeOption(i: CreateOptionsInput): CreateOption {
    if (!i.gatewayEnabled) {
        return {
            enabled: false,
            reason: 'Gateway routing is turned off on this platform.',
        };
    }
    if (!i.routeOnlyEnabled) {
        return { enabled: false, reason: 'Protected addresses are turned off on this platform.' };
    }
    if (i.isAdmin) return { enabled: true, href: '/routes' };
    if (i.entitledRouteOnly) return { enabled: true, href: '/routes' };
    return {
        enabled: false,
        reason: 'Your account does not include protected addresses yet.',
        ...(i.storeEnabled
            ? { href: '/nodes', hrefLabel: 'How to get it' }
            : { reason: 'Your account does not include protected addresses. Ask an admin to enable it.' }),
    };
}

/** True when the "+" has nothing usable at all, so it can be hidden entirely. */
export function hasAnyCreateOption(i: CreateOptionsInput): boolean {
    return serverOption(i).enabled || nodeOption(i).enabled || routeOption(i).enabled;
}
