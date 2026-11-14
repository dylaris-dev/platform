// Which gateway this platform uses, and what the panel shows because of it.
//
// "local" is the platform's own gateway (Hub, edges, links, warp). "remote" is
// the DYLARIS Managed Gateway: another Core runs the gateway, and the routes
// that read or write local gateway state answer 409 gateway_remote. The panel
// learns the mode once, from GET /system/features, and every surface asks
// visibilityFor rather than checking the mode itself, so the list of what is
// hidden lives in one place.

export type GatewayProviderMode = 'local' | 'remote';

export interface RemoteVisibility {
    hubDnsCard: boolean;
    hosterDomainEditor: boolean;
    routeLimits: boolean;
    warpTab: boolean;
    voice: boolean;
    bedrock: boolean;
    edgeMotd: boolean;
    infrastructureGateway: boolean;
    playerCounts: boolean;
    // BYON and route-only: tenant nodes, link routes, public API targets.
    tenancy: boolean;
    managedLinkKit: boolean;
    providerUsage: boolean;
}

/**
 * The mode as the features map reports it. Anything but "remote" is local: an
 * older Core omits the field, and Core itself reports an unreadable mode as
 * local.
 */
export function providerModeOf(flags: { gatewayProvider?: string } | null | undefined): GatewayProviderMode {
    return flags?.gatewayProvider === 'remote' ? 'remote' : 'local';
}

export function visibilityFor(mode: GatewayProviderMode): RemoteVisibility {
    const local = mode === 'local';
    return {
        hubDnsCard: local,
        hosterDomainEditor: local,
        // Per-user route limits are enforced by this Core in both modes; the
        // provider's own cap sits on top of them.
        routeLimits: true,
        warpTab: local,
        voice: local,
        bedrock: local,
        edgeMotd: local,
        infrastructureGateway: local,
        playerCounts: local,
        tenancy: local,
        managedLinkKit: !local,
        providerUsage: !local,
    };
}

/** What a hidden gateway screen says instead, when it is reached by address. */
export const MANAGED_GATEWAY_NOTE =
    'This platform uses the DYLARIS Managed Gateway, which runs this part of the gateway. There is nothing to manage here.';

const PROVIDER_ERRORS: Record<string, string> = {
    unauthorized: 'The provider rejected this key. Check that it was copied in full and has not been revoked.',
    forbidden: 'The provider refused this key. The account may be suspended, or the key lacks the gateway permission.',
    no_entitlement: 'This provider account has no managed gateway plan.',
    billing_suspended: 'The provider account is suspended for billing. Settle it in the DYLARIS store to continue.',
    rate_limited: 'The provider is limiting requests. Try again in a minute.',
    unreachable: 'The provider could not be reached. Check the address and this server\'s outbound connection.',
    gateway_remote: 'This is run by the managed gateway and cannot be changed here.',
    no_key: 'Enter the API key. A new provider address needs the key typed in again.',
    key_from_env: 'The key is set by the environment (GATEWAY_PROVIDER_KEY), so it cannot be changed here.',
    url_from_env: 'The address is set by the environment (GATEWAY_PROVIDER_URL), so it cannot be changed here.',
    invalid_url: 'The address must be https:// with a host only, no path.',
    not_managed: 'This provider account has no managed gateway plan.',
    account_suspended: 'This provider account is suspended.',
    missing_scope: 'This key lacks the gateway.write permission.',
    tenancy_enabled: 'Turn off BYON and route-only under Features first. They need the platform\'s own gateway.',
    routing_mode: 'Switch the routing mode to Gateway or Both first. Servers get their addresses from the gateway only in those modes.',
    provider_error: 'The provider answered with an error. Try again, or contact DYLARIS support if it persists.',
    provider_key_rejected: 'The provider rejected the API key. Check it under Settings, Gateway.',
    provider_unreachable: 'The provider could not be reached. Try again in a minute.',
    gateway_local: 'The managed gateway is not enabled on this platform.',
    gateway_provider_unknown: 'The gateway provider setting could not be read. Try again in a moment.',
    retire_limit: 'All replacement slots of this plan are in use. Wait until a link being replaced has run out.',
    retire_budget: "This plan's monthly number of link replacements is used up.",
    retire_not_needed: 'This plan has no host limit, so no link needs replacing: add the new node and move the servers to it.',
    kit_retiring: "This node's link is being replaced. Move the server to another node first.",
    kit_known: 'This link belongs to a node of this platform. Replace or delete that node instead.',
    no_kit: 'This node has no managed link yet.',
    link_limit: "This plan's host limit is reached. Remove a node's link, or use Replace this node's link on a node you are retiring.",
    kit_already_retiring: "This node's link is already being replaced.",
    // Own domains. custom_domain_cap is this platform's per-user cap; the
    // other two limits are the provider plan's, shared by the whole platform.
    domain_unverified: 'This domain is not verified yet. Publish both TXT records shown under Your own domains, then check again.',
    domain_blocked: 'This domain is blocked after failed ownership checks. Check it again under Your own domains.',
    custom_domain_cap: 'You have reached your limit of routes on your own domains.',
    custom_domain_limit: "This platform's own-domain limit at the gateway provider is reached. Ask the platform operator.",
    too_many_domains: "This platform's own-domain limit at the gateway provider is reached. Ask the platform operator.",
    custom_domains_off: 'Own domains are not enabled for this account on the managed gateway.',
    domain_in_use: 'Delete the routes on this domain first.',
    domain_taken: 'This address is already in use.',
    change_pending: 'A change to this domain is still on its way to the gateway provider. Try again in a minute.',
    verify_cooldown: 'This domain was checked a moment ago. Try again in a few minutes.',
    pending_domains_limit: 'Finish verifying your pending domains before adding another.',
};

/** The sentence for a refusal code; an unknown code gets `fallback`, else a generic sentence. */
export function providerErrorText(code: string, fallback?: string): string {
    return PROVIDER_ERRORS[code] ?? fallback ?? 'Something went wrong talking to the gateway provider. Try again in a moment.';
}

/** An open outbox is worth a warning only once it has stopped draining on its own. */
export const OUTBOX_STALE_MS = 2 * 60 * 1000;

export function outboxWarning(outbox: { open: number; oldestAt?: string } | null | undefined, now: number): string | null {
    if (!outbox || outbox.open <= 0 || !outbox.oldestAt) return null;
    const oldest = Date.parse(outbox.oldestAt);
    if (Number.isNaN(oldest) || now - oldest <= OUTBOX_STALE_MS) return null;
    return outbox.open === 1
        ? '1 change is waiting to reach the gateway provider.'
        : `${outbox.open} changes are waiting to reach the gateway provider.`;
}

/**
 * The card's routing mode warning: the managed gateway is chosen or on while
 * servers take their addresses from IP:Port only. undefined (an older Core)
 * says nothing.
 */
export function routingModeWarning(routingModeOk: boolean | undefined, chosen: boolean, saved: boolean): string | null {
    return routingModeOk === false && (chosen || saved) ? providerErrorText('routing_mode') : null;
}

const OUTBOX_OPS: Record<string, string> = {
    proxy: 'Real player IP',
    move: 'Move',
    delete: 'Delete',
    revoke_kit: 'Revoke a link',
    delete_custom_domain: 'Release an own-domain proof at the gateway provider',
};

/** One change the provider refused, as the card lists it. */
export function refusalLine(item: { op: string; domain: string; error: string }): string {
    return `${OUTBOX_OPS[item.op] ?? item.op} for ${item.domain}: ${item.error}`;
}

/** The confirm before a node's link is replaced. The provider's window is 8 hours. */
export const RETIRE_CONFIRM = {
    title: "Replace this node's link?",
    message: 'The link keeps working for 8 hours so players can finish, then it is removed. '
        + "During that time another node can get a link even if your plan's host limit is reached. "
        + "Move this node's servers to the new node before the time is up. This cannot be undone.",
    confirmLabel: 'Replace the link',
    destructive: true,
    focusCancel: true,
};

/** A link can be replaced once; one already being replaced shows its deadline instead. */
export function canReplaceLink(kit: { retireAt?: string } | null | undefined): boolean {
    return !!kit && !kit.retireAt;
}

/** What the node's kit says while its link is being replaced. */
export function retiringLine(retireAt: string, fmt: (v: string) => string): string {
    return `Being replaced. Removed at ${fmt(retireAt)}.`;
}

/** The cleanup view's caveat: the provider cannot tell two installations on one account apart. */
export const ORPHAN_INTRO = 'These links exist on the gateway provider, but no node of this platform has them, for example after the database was lost or restored. '
    + 'Only revoke a link here if no other DYLARIS installation uses this provider account.';

/** The cleanup view shows only when there is something to clean up, or its list failed. */
export function showOrphanSection(items: unknown[] | null | undefined, error: string): boolean {
    return !!error || (!!items && items.length > 0);
}

export function orphanRevokeConfirm(name: string) {
    return {
        title: 'Revoke this link?',
        message: `${name ? `"${name}"` : 'This link'} stops connecting at once, and every address on it is deleted with it. `
            + 'If another DYLARIS installation uses this provider account, its node loses its link.',
        confirmLabel: 'Revoke',
        destructive: true,
        focusCancel: true,
    };
}

/** The provider's percentage, clamped for a bar width. */
export function barPercent(percent: number | null | undefined): number {
    if (typeof percent !== 'number' || !Number.isFinite(percent)) return 0;
    return Math.max(0, Math.min(100, Math.round(percent)));
}

// Bare words YAML 1.1 reads as something other than a string. The compose
// quotes the name anyway; refusing them keeps a hand-edited copy safe too.
const YAML_AMBIGUOUS = new Set(['null', 'true', 'false', 'yes', 'no', 'on', 'off', 'y', 'n', '~']);

// Mirrored by pkg/validate IsDockerNetworkName, which Core applies to the name
// a node reports; validationParity.test.ts pins the two patterns.
export const DOCKER_NETWORK_NAME = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,126}$/;
export const DIGITS_AND_PUNCT = /^[0-9._-]+$/;

/** A plain Docker network name: what the managed link compose may carry. */
export function isDockerNetworkName(s: string): boolean {
    if (!DOCKER_NETWORK_NAME.test(s)) return false;
    if (YAML_AMBIGUOUS.has(s.toLowerCase())) return false;
    return !DIGITS_AND_PUNCT.test(s);
}

/**
 * The deploy form's network: the name the node reported, read-only, when Core
 * says it is known and it passes the same check the field applies; otherwise
 * Core's hint as an editable start.
 */
export function initialNetwork(kit: { network?: string; networkKnown?: boolean; networkHint?: string }): { value: string; known: boolean } {
    if (kit.networkKnown && kit.network && isDockerNetworkName(kit.network)) return { value: kit.network, known: true };
    return { value: kit.networkHint ?? '', known: false };
}

const SWITCH_TEXT: Record<GatewayProviderMode, { title: string; message: string }> = {
    remote: {
        title: 'Switch to the DYLARIS Managed Gateway?',
        message: 'Players then reach your servers through the managed gateway, and each node needs its managed link running. The screens for your own edges, warp, voice and Bedrock are hidden while it is on.',
    },
    local: {
        title: "Switch back to this platform's own gateway?",
        message: 'Your own Hub, edges and links carry the players again, and the managed links stop being used. Nothing is deleted on this platform.',
    },
};

/**
 * The confirm a provider save asks for: only a mode change, which changes how
 * every player connects. Fed to useSettingsForm, so every save path asks,
 * including the leave-with-unsaved-changes dialog.
 */
export function switchConfirm(next: { mode: GatewayProviderMode }, prev: { mode: GatewayProviderMode }):
    { title: string; message: string; confirmLabel: string; destructive: boolean; focusCancel: boolean } | null {
    if (next.mode === prev.mode) return null;
    // Cancel first: the switch moves every player's path.
    return { ...SWITCH_TEXT[next.mode], confirmLabel: 'Switch', destructive: false, focusCancel: true };
}

const PRODUCTS: Record<string, string> = {
    managed: 'Managed gateway',
    route_only: 'Route-only',
};

/** The provider's product id as a reader would name it; an unknown id as sent. */
export function productLabel(id: string | undefined): string {
    if (!id) return 'Unknown';
    return PRODUCTS[id] ?? id;
}

/**
 * The account the card shows. After a test it is the TESTED one, or none:
 * a failed test beside the saved account's summary would contradict itself.
 */
export function testedAccount<A>(test: { account?: A } | null, saved: A | undefined): A | undefined {
    return test ? test.account : saved;
}

/** The provider counts traffic in decimal GB (1 TB = 1000 GB). */
export function providerGbToBytes(gb: number): number {
    return gb * 1e9;
}

/** Only an answer that says it is final is final; anything else is an estimate. */
export function usageFinalLabel(final: boolean | undefined): string {
    return final === true ? 'Final' : 'Estimate, the month is still running';
}

/** What may still be bought on top of the pool. null = no purchase cap. */
export function purchaseNote(maxPurchaseGb: number | null | undefined, fmt: (bytes: number) => string): string {
    if (maxPurchaseGb === 0) return 'No extra traffic can be bought.';
    if (maxPurchaseGb === null || maxPurchaseGb === undefined) return 'More can be bought.';
    return `Up to ${fmt(providerGbToBytes(maxPurchaseGb))} more can be bought.`;
}
