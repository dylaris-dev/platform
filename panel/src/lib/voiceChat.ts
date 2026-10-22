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
};

export function voiceErrorMessage(code: string | undefined, fallback: string): string {
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
