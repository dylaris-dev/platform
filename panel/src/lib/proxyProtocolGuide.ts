// What a server must be told before the gateway may send it the real player IP
// (a PROXY v2 header). v1 guides instead of writing the file: a write racing a
// restart would be a second way to lock every player out. Table: spec A6
// (docs/superpowers/specs/2026-10-18-real-player-ip-and-udp-design.md).

export interface ProxyProtocolGuide {
    /** false: the software cannot read the header at all, so the switch is disabled. */
    supported: boolean;
    file?: string;
    /** Where in the file the line goes, as a phrase: "in the proxies: section". */
    where?: string;
    /** The exact text to put there, shown on its own so it copies cleanly. */
    line?: string;
    /** Install ONE of these instead of editing a file. */
    mods?: ProxyMod[];
}

/** A server-side mod that reads the PROXY header, with where to get it. */
export interface ProxyMod {
    name: string;
    url: string;
    /** Which Minecraft or loader versions it covers, when that is narrower than the loader. */
    note?: string;
}

// Sources: each mod's Modrinth page, read 2026-11-02.
export const HAPROXYCOMPAT: ProxyMod = { name: 'HAProxyCompat', url: 'https://modrinth.com/mod/haproxycompat' };
export const PROXY_PROTOCOL_SUPPORT: ProxyMod = { name: 'Proxy Protocol Support', url: 'https://modrinth.com/mod/proxy-protocol-support' };
export const NG_PROXY_SUPPORT: ProxyMod = { name: 'NG Proxy Support', url: 'https://modrinth.com/mod/ng-haproxy-support', note: 'Minecraft 1.20.1' };

const GUIDES: Record<string, ProxyProtocolGuide> = {
    paper: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' },
    purpur: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' },
    folia: { supported: true, file: 'config/paper-global.yml', where: 'in the proxies: section', line: 'proxy-protocol: true' },
    velocity: { supported: true, file: 'velocity.toml', where: 'in the [advanced] section', line: 'haproxy-protocol = true' },
    bungeecord: { supported: true, file: 'config.yml', where: 'on the listener', line: 'proxy_protocol: true' },
    waterfall: { supported: true, file: 'config.yml', where: 'on the listener', line: 'proxy_protocol: true' },
    fabric: { supported: true, mods: [HAPROXYCOMPAT, PROXY_PROTOCOL_SUPPORT] },
    neoforge: { supported: true, mods: [HAPROXYCOMPAT] },
    forge: { supported: true, mods: [NG_PROXY_SUPPORT] },
    vanilla: { supported: false },
};

/**
 * Unknown software (an upload, a modpack, spigot) gets `{ supported: true }`
 * with no file: the panel cannot tell what it runs, so it shows the generic
 * text and leaves the decision to the owner.
 */
export function proxyProtocolGuide(software: string): ProxyProtocolGuide {
    return GUIDES[(software || '').trim().toLowerCase()] ?? { supported: true };
}

export const PROXY_PROTOCOL_RESTART = 'Restart the server after changing its config.';
export const PROXY_PROTOCOL_SECURITY =
    'Only enable this when the server is reachable only through the gateway: anyone who can reach its port directly could claim any IP.';
export const PROXY_PROTOCOL_LINK =
    "This server's link must run the latest version. Through an older link nobody can join once this is on.";
export const PROXY_PROTOCOL_DISABLE =
    'Players connect without the PROXY header again. If the server still expects it, nobody can join until you turn its proxy setting off too.';
export const PROXY_PROTOCOL_VANILLA =
    'Vanilla servers cannot read the PROXY header, so the real player IP cannot be passed to them. Switch to Paper, Fabric or another supported software to use this.';

export function modLabel(m: ProxyMod): string {
    return m.note ? `${m.name} (${m.note})` : m.name;
}

function configStep(g: ProxyProtocolGuide): string {
    if (g.file && g.line) {
        // The value is its own paragraph with no punctuation after it: owners
        // copy it straight into the file, and a trailing period breaks YAML/TOML.
        return `In ${g.file}, ${g.where}, set:\n\n${g.line}`;
    }
    if (g.mods?.length) return `Install one of these mods: ${g.mods.map(modLabel).join(', ')}. Add the gateway's address range to its trusted proxies.`;
    return 'Configure the server to expect a PROXY protocol v2 header first. Without that, nobody can join once this is on.';
}

/** The confirmation text for turning forwarding ON, one paragraph per line. */
export function proxyProtocolEnableMessage(software: string, viaLink: boolean): string {
    const parts = [configStep(proxyProtocolGuide(software)), PROXY_PROTOCOL_RESTART, PROXY_PROTOCOL_SECURITY];
    if (viaLink) parts.push(PROXY_PROTOCOL_LINK);
    return parts.join('\n\n');
}

/** The status line under the mode control: what forwarding does right now and why. */
export function proxyModeStatus(mode: 'auto' | 'on' | 'off', active: boolean, detected: 'yes' | 'no' | 'unknown'): string {
    if (mode === 'off') return 'Off: players reach the server without their IP.';
    if (mode === 'on') return active
        ? 'Always on: every connection carries the player IP. The server must stay set up to expect it.'
        : 'Always on was chosen, but the gateway has not applied it yet. Save again.';
    if (active) return 'Forwarding: the server showed that it expects the player IP.';
    if (detected === 'no') return 'Not forwarding: the server does not expect the player IP. Install a PROXY protocol plugin or mod (or turn it on in the config) and restart; it is picked up within a minute.';
    return 'Not forwarding yet: the server has not been checked. Start it; it is picked up within a minute of starting.';
}


/**
 * The private ranges the gateway's connection arrives from. On a DYLARIS node
 * the link reaches the server over the node's Docker network, and its address
 * there changes whenever the link is updated, so a single IP would go stale;
 * on your own machine it is 127.0.0.1 or a LAN address. Docker draws every
 * network from these ranges. Trusting them is safe only because the server's
 * port is reachable through the gateway alone (no published host port).
 */
export const PROXY_TRUSTED_RANGES = ['127.0.0.1/32', '10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16'];

/** One row of the per-software setup list in the panel and the docs. */
export interface ProxySetupRow {
    label: string;
    /** installerType values this row is about, so the panel can mark the server's own. */
    software: string[];
    text: string;
    file?: string;
    line?: string;
    mods?: ProxyMod[];
}

export const PROXY_SETUP_ROWS: ProxySetupRow[] = [
    { label: 'Paper, Purpur, Folia', software: ['paper', 'purpur', 'folia'], text: 'Built in.', file: 'config/paper-global.yml, in the proxies: section', line: 'proxy-protocol: true' },
    { label: 'Velocity', software: ['velocity'], text: 'Built in.', file: 'velocity.toml, in the [advanced] section', line: 'haproxy-protocol = true' },
    { label: 'BungeeCord, Waterfall', software: ['bungeecord', 'waterfall'], text: 'Built in.', file: 'config.yml, on the listener', line: 'proxy_protocol: true' },
    { label: 'Fabric', software: ['fabric'], text: 'Install one mod and trust the gateway in its config.', mods: [HAPROXYCOMPAT, PROXY_PROTOCOL_SUPPORT] },
    { label: 'NeoForge 1.21 and newer', software: ['neoforge'], text: 'Install the mod and trust the gateway in its config.', mods: [HAPROXYCOMPAT] },
    { label: 'Forge 1.20.1', software: ['forge'], text: 'Install the mod and trust the gateway in its config.', mods: [NG_PROXY_SUPPORT] },
    { label: 'Vanilla, Spigot', software: ['vanilla', 'spigot'], text: 'Cannot read the header. Switch to Paper (plugins) or Fabric (mods) to get real player IPs.' },
];
