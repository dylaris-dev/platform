// The host a DIRECT custom tab renders, for the notice shown above its frame.
//
// It exists because a custom tab's URL is not the owner's word alone: tabs.write
// is in the "Server admin" preset, so anyone the owner delegated that to can
// point a tab anywhere, and the panel then renders it full-bleed with scripts
// and forms enabled and nothing on screen saying whose page it is. The address
// bar still reads the panel's own domain. Naming the host is what turns that
// from an impersonation into a visit.
//
// Only http(s) yields a host. Core's validateTabURL already refuses anything
// else, and this is the second place that has to be true rather than assumed.
// An unparseable or non-web URL returns "", which the notice renders as a
// generic warning instead of dropping itself - a tab whose origin cannot be
// named is the one most worth warning about.
export function tabHost(url: string): string {
    try {
        const u = new URL(url);
        if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
        return u.host;
    } catch {
        return '';
    }
}
