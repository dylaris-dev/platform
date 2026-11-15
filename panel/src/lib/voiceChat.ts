// Pure helpers for the per-server voice chat switch (RoutesModal).

/** One actionable sentence per refusal Core answers the enable with (409).
 *  Core's own message is the fallback for anything else (502, 403, ...). */
const VOICE_ERRORS: Record<string, string> = {
    voice_not_allowed: 'Voice chat is not enabled for this server. Ask an admin.',
    node_too_old: "Update this server's node, then try again.",
    no_routes: "Add a route on one of the platform's domains first, then try again.",
    no_relay: "No voice relay runs in this server's region yet. Try again later or ask an admin.",
    no_sub_server: 'Install the server first, then try again.',
    voice_mod_missing: 'Install Simple Voice Chat or Plasmo Voice, start the server once, then try again.',
    voice_mod_ambiguous: 'Both Simple Voice Chat and Plasmo Voice are installed. Remove one, then try again.',
    voice_pool_full: 'Every voice port in this region is taken. Ask an admin.',
    // Managed gateway only; that mode shows Core's own sentence first.
    voice_one_address: 'With the managed gateway a server has one voice chat address. Turn voice chat off, then on again.',
    link_without_voice: "This route's link has not reported that it can carry voice chat. Make sure it is running and up to date (watchtower updates it nightly), then try again.",
};

// The start of Core's no_routes for a server or route without any address on
// our domains. Its other no_routes sentences are about an own domain's DNS and
// name what to change, which the table cannot.
const CORE_NO_ROUTES = "Voice chat needs an address on one of the platform's domains";

export function voiceErrorMessage(code: string | undefined, fallback: string): string {
    if (code === 'no_routes' && !fallback.startsWith(CORE_NO_ROUTES)) return fallback;
    return (code && VOICE_ERRORS[code]) || fallback;
}

/** The address players' voice clients connect to. */
export function voiceAddress(v: { host: string; port: number }): string {
    return `${v.host}:${v.port}`;
}

/** Hidden unless this server may turn voice on, except while it is on: an
 *  owner must always be able to turn it off again. */
export function showVoiceSwitch(allowed: boolean, enabled: boolean): boolean {
    return allowed || enabled;
}

/** The name a voice mod is told: the host Core answers (our regional voice
 *  name for an address on an own domain, which may reach us by SRV only, so
 *  the address itself would not resolve for voice clients). Only an older
 *  Core sends none, and then it is the address. */
export function voiceTargetHost(voice: { host?: string } | undefined, domain: string): string {
    return voice?.host || domain;
}

/** The managed gateway's help line under the voice chat switch. */
export function managedVoiceHelp(enabled: boolean): string {
    return "Voice chat runs through the managed gateway's relay, and a server has one voice address. " +
        "On your own domain, the voice address is on the gateway provider's domain; your DNS needs no change. " +
        'Restart the server after switching it on.' +
        (enabled ? '' : " If it went off on its own, the managed gateway's side changed: switch it on again, then restart.");
}

/** The Voice chat access card's help line. On the managed gateway a deny acts
 *  at the platform's next check of the managed gateway (every five minutes),
 *  not within the minute the own gateway's reconcile takes. */
export function voiceAccessDescription(managed: boolean): string {
    const when = managed
        ? "at this platform's next check of the managed gateway, within about five minutes"
        : 'within a minute';
    return `Overrides the voice chat switch above for one server, user or role. Server beats user beats role. A deny turns voice off on servers that have it ${when}.`;
}

/** What a route-only owner sets in their voice mod's config by hand: Core
 *  cannot write files on their machine. Mirrors what Core writes for a hosted
 *  server (handlers/voice_config.go): the mod listens on targetPort, and
 *  tells clients to connect to domain:port. */
export function linkRouteVoiceConfig(domain: string, port: number, targetPort: number): { svc: string; plasmo: string } {
    return {
        svc: `port=${targetPort}
voice_host=${domain}:${port}`,
        plasmo: `[host]
port = ${targetPort}

[host.public]
ip = "${domain}"
port = ${port}`,
    };
}

/** A voice port is 1-65535; anything else is refused before the request. */
export function validVoicePort(n: number): boolean {
    return Number.isInteger(n) && n >= 1 && n <= 65535;
}
