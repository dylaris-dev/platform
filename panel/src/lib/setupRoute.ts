import type { CreateRouteRequest } from '@/lib/api';

// What the setup form's domain fields mean for the route created right after
// setup: the effective domain, and whether a NEW route will be created.
// Core needs the second ahead of the setup (createsRoute): only a new server
// that gets a route defaults to real player IP, because a server without one
// is likely a proxy's backend and the proxy dials it without the header.
export function plannedSetupRoute(route: Partial<CreateRouteRequest>, existing: { domain: string }[]) {
    const hasDomain = !!(route.subdomain || route.customDomain || route.domain);
    // Mirrors the picker's preview computation so the comparison sees the
    // same string that ends up in createServerRoute.
    const effectiveDomain = (route.subdomain && route.hosterDomain)
        ? `${route.subdomain}.${route.hosterDomain}`.toLowerCase()
        : (route.customDomain || route.domain || '').toLowerCase();
    const alreadyOurs = !!effectiveDomain && existing.some(r => r.domain.toLowerCase() === effectiveDomain);
    return { hasDomain, effectiveDomain, alreadyOurs, createsRoute: hasDomain && !alreadyOurs };
}
