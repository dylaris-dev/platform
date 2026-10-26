// Helpers for the proxy (BungeeCord/Velocity) Config-tab helper.
//
// The proxy config filename is software-dependent: Velocity uses velocity.toml
// (TOML), while BungeeCord and Waterfall use config.yml (YAML). Backends are
// reached in-network by the stable Docker DNS alias mc-<uuid> on the container
// port (default 25565). That name survives restarts and sub-server switches -
// unlike a dynamic container IP - so it is safe to paste into a proxy config.
// It is not the container name mc_<uuid>: the underscore makes that an invalid
// hostname, and Velocity refuses to start with it in velocity.toml.

const DEFAULT_CONTAINER_PORT = 25565;

export function proxyConfigFilename(installerType: string | undefined | null): string {
    return (installerType || '').toLowerCase() === 'velocity' ? 'velocity.toml' : 'config.yml';
}

// Mirrors validate.ServerHostname in platform/pkg, which names the alias the
// node registers. A UUID that cannot form a 63-byte DNS label has no alias.
export function serverHostname(uuid: string): string {
    if (!/^[a-zA-Z0-9_-]{8,80}$/.test(uuid)) return '';
    const name = 'mc-' + uuid.replace(/_/g, '-').toLowerCase();
    return name.length > 63 || name.endsWith('-') ? '' : name;
}

export function backendAddress(uuid: string, containerPort?: number | null): string {
    const port = containerPort && containerPort > 0 ? containerPort : DEFAULT_CONTAINER_PORT;
    return `${serverHostname(uuid) || `mc_${uuid}`}:${port}`;
}

export function proxyPrereqHint(installerType: string | undefined | null): string {
    if ((installerType || '').toLowerCase() === 'velocity') {
        return 'Each backend needs online-mode=false, enforce-secure-profile=false, and Velocity modern forwarding with a matching forwarding.secret.';
    }
    return 'Each backend needs online-mode=false and BungeeCord support enabled (Spigot spigot.yml bungeecord: true, or the Paper equivalent).';
}
