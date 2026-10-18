// What a server must be told before the gateway may send it the real player IP
// (a PROXY v2 header). v1 guides instead of writing the file: a write racing a
// restart would be a second way to lock every player out. Table: spec A6
// (docs/superpowers/specs/2026-10-18-real-player-ip-and-udp-design.md).

export interface ProxyProtocolGuide {
    /** false: the software cannot read the header at all, so the switch is disabled. */
    supported: boolean;
    file?: string;
    line?: string;
    /** Install ONE of these instead of editing a file. */
    mods?: string[];
}

const GUIDES: Record<string, ProxyProtocolGuide> = {
    paper: { supported: true, file: 'config/paper-global.yml', line: 'proxies.proxy-protocol: true' },
    purpur: { supported: true, file: 'config/paper-global.yml', line: 'proxies.proxy-protocol: true' },
    velocity: { supported: true, file: 'velocity.toml', line: 'haproxy-protocol = true' },
    bungeecord: { supported: true, file: 'config.yml', line: 'proxy_protocol: true' },
    waterfall: { supported: true, file: 'config.yml', line: 'proxy_protocol: true' },
    fabric: { supported: true, mods: ['Proxy Protocol Support', 'HAProxyCompat'] },
    neoforge: { supported: true, mods: ['HAProxyCompat', 'ProperProxyProtocol'] },
    forge: { supported: true, mods: ['ProperProxyProtocol', 'NG Proxy Support (Minecraft 1.20.1)'] },
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
export const PROXY_PROTOCOL_LINK = 'Your link must be updated to pass the player IP.';
export const PROXY_PROTOCOL_DISABLE =
    'Players connect without the PROXY header again. If the server still expects it, nobody can join until you turn its proxy setting off too.';
export const PROXY_PROTOCOL_VANILLA =
    'Vanilla servers cannot read the PROXY header, so the real player IP cannot be passed to them. Switch to Paper, Fabric or another supported software to use this.';

function configStep(g: ProxyProtocolGuide): string {
    if (g.file && g.line) {
        // BungeeCord's setting lives on the listener, not at the top of the file.
        const where = g.line.startsWith('proxy_protocol') ? ' on the listener' : '';
        return `In ${g.file}, set ${g.line}${where}.`;
    }
    if (g.mods?.length) return `Install one of these mods: ${g.mods.join(', ')}.`;
    return 'Configure the server to expect a PROXY protocol v2 header first. Without that, nobody can join once this is on.';
}

/** The confirmation text for turning forwarding ON, one paragraph per line. */
export function proxyProtocolEnableMessage(software: string, viaLink: boolean): string {
    const parts = [configStep(proxyProtocolGuide(software)), PROXY_PROTOCOL_RESTART, PROXY_PROTOCOL_SECURITY];
    if (viaLink) parts.push(PROXY_PROTOCOL_LINK);
    return parts.join('\n\n');
}
