// Pure helpers for the per-server voice chat switch (RoutesModal).

import { fmtDate, fmtDateTime } from './time';

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

/** The name a voice mod is told: Core's mod_host (the white-label name while
 *  it is active, our name again after a fall back), else the host Core
 *  answers (our regional voice name for an address on an own domain, which
 *  may reach us by SRV only, so the address itself would not resolve for
 *  voice clients). Only an older Core sends neither, and then it is the
 *  address. */
export function voiceTargetHost(voice: { host?: string; mod_host?: string } | undefined, domain: string): string {
    return voice?.mod_host || voice?.host || domain;
}

export type VoiceNameAction = 'useOwn' | 'check' | 'cancel' | 'remove' | 'restart';

/** A white-label voice name as the voice card reads it. ourHost is our
 *  regional name, which the mod gets back on a cancel or a fall back. target
 *  is Core's CNAME target; absent (an older Core), it is ourHost, which the
 *  check also accepts. lastCheck is the sentence of a check this view made.
 *  Without failingSince an active name reads as healthy, and without
 *  fallenBackAt the fall back has no date (an older Core). */
export interface VoiceNameInput {
    state?: string;
    ourHost: string;
    name?: string;
    port: number;
    target?: string;
    lastCheck?: string;
    failingSince?: string | null;
    fallenBackAt?: string | null;
    routeOnly?: boolean;
}

export interface VoiceNameView {
    tone: 'neutral' | 'info' | 'success' | 'warning';
    text: string;
    actions: VoiceNameAction[];
}

/** Whether the white-label name block shows. Core's name target decides: ""
 *  means no name is possible here (an address on our domains, a server without
 *  an own domain). A name that exists always shows, so it can be removed.
 *  legacy is the guess for an older Core that sends no target. */
export function showVoiceName(state: string | undefined, nameTarget: string | undefined, legacy: boolean): boolean {
    if (state) return true;
    return nameTarget === undefined ? legacy : nameTarget !== '';
}

/** The fall back date: Core's state time, only while the state is fallen_back. */
export function fallenBackAt(state: string | undefined, at: string | null | undefined): string | undefined {
    return state === 'fallen_back' && at ? at : undefined;
}

/** The voice card's line for the white-label name (spec 7.5). The state is
 *  read before failingSince: a stamp on a row that is not active means
 *  nothing here. */
export function voiceNameState(v: VoiceNameInput): VoiceNameView {
    const name = v.name || '';
    switch (v.state) {
    case 'pending': {
        const last = v.lastCheck ? ` Last check: ${v.lastCheck}${/[.!?]$/.test(v.lastCheck) ? '' : '.'}` : '';
        return {
            tone: 'info',
            text: `Waiting for your DNS record. Create: ${name} CNAME ${v.target || v.ourHost}.${last}`,
            actions: ['check', 'cancel'],
        };
    }
    case 'active':
        if (v.failingSince) {
            return {
                tone: 'warning',
                text: `Your record ${name} stopped pointing at the gateway at ${fmtDateTime(v.failingSince)}. The gateway is checking again. If it stays that way, voice chat switches back to ${v.ourHost}.`,
                actions: ['remove'],
            };
        }
        return { tone: 'success', text: `Voice chat address: ${name}:${v.port} (your own domain).`, actions: ['remove'] };
    case 'fallen_back': {
        const on = v.fallenBackAt ? ` on ${fmtDate(v.fallenBackAt)}` : '';
        // Core cannot write a route-only owner's config or restart their server.
        const apply = v.routeOnly
            ? "Change the voice host in your voice mod's config to the line below by hand, then restart the server."
            : 'Restart the server to apply it.';
        return {
            tone: 'warning',
            text: `Voice chat was switched back to ${v.ourHost}${on} because ${name} no longer points at the gateway. ${apply} Fix the record, then press Check again to use your own name.`,
            actions: v.routeOnly ? ['check', 'remove'] : ['restart', 'check', 'remove'],
        };
    }
    default:
        return {
            tone: 'neutral',
            text: `Voice chat address: ${v.ourHost}:${v.port}. Players keep joining through your own address.`,
            actions: ['useOwn'],
        };
    }
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
