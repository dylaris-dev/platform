// Which part of "my infrastructure" to show.
//
// Extracted because this expression has been wrong three times: once honouring
// ?tab=routes on a platform with no gateway routing (opening a panel whose
// endpoints 409), once stranding route-only customers on a platform with BYON
// off, where the page returned "your own hardware is turned off" with the routes
// tab unreachable behind it, and once showing every swarm host under "my
// machines" because an admin's node list was never scoped.

export type InfraTab = 'external' | 'machines' | 'routes';

export interface InfraAvailability {
    /**
     * Admin only: the operator's OWN machines outside the swarm. Not a feature
     * flag - it is the operator's half of the same page, and a tenant has no
     * business seeing it. Core enforces the same rule on ?scope=external, so a
     * hand-edited URL gets a 403 rather than a list.
     */
    external: boolean;
    /** feature_byon_enabled: this platform runs servers on tenant machines. */
    machines: boolean;
    /**
     * Protected addresses exist AND can be minted. This is byonEnabled, not
     * gatewayEnabled: Core's MintLinkKit and ListNodeWarpKeys refuse on
     * byonActive BEFORE they look at the routing mode. Asking routing alone showed the whole route-only half on a
     * gateway-routed install with BYON off, where every read under it answered
     * 403 "BYON is not enabled" - to admins included.
     */
    routes: boolean;
}

/**
 * Builds the map from the reader's role and the ONE platform predicate that
 * governs tenant hardware. byonEnabled is already `feature_byon_enabled AND
 * gateway routing` (see isByonUsable), which is byte-for-byte what Core checks.
 *
 * Kept here rather than inline in the page because the same flags decide three
 * different things and the page has picked the wrong pair before - see the
 * history at the top of this file.
 */
export function infraAvailability(isAdmin: boolean, byonEnabled: boolean, routeOnlyEnabled: boolean = byonEnabled): InfraAvailability {
    // Route-only has its own switch; unset it follows BYON, so an install that
    // never set it gets the same two halves as before.
    return { external: isAdmin, machines: byonEnabled, routes: routeOnlyEnabled };
}

const ORDER: InfraTab[] = ['external', 'machines', 'routes'];

function availableTabs(have: InfraAvailability): InfraTab[] {
    return ORDER.filter(t => have[t]);
}

/**
 * Resolves the tab against what the reader actually HAS, never against the URL
 * alone. Entitlement is a separate question the panels answer for themselves -
 * someone unentitled still gets the tab, and an explanation inside it.
 *
 * An unavailable tab falls through to the first available one rather than
 * erroring: that is the redirect a non-admin gets when they type ?tab=external.
 *
 * Returns null when nothing is available: the caller has nothing to show and
 * says so instead of rendering an empty tab.
 */
export function resolveInfraTab(requested: string | null, have: InfraAvailability): InfraTab | null {
    const tabs = availableTabs(have);
    if (tabs.length === 0) return null;
    const wanted = tabs.find(t => t === requested);
    // First available, in ORDER - which is what makes external the admin default
    // and machines the tenant one, without either being special-cased.
    return wanted ?? tabs[0];
}

/** The bar only earns its space when there is a choice to make. */
export function showInfraTabBar(have: InfraAvailability): boolean {
    return availableTabs(have).length > 1;
}
