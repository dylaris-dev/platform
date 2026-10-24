// Does an installed build fit this server? The panel's copy of Core's
// services/mod_fit.go, used to MARK rows; Core still decides every install.
// Keep the two tables equal: a row marked here that Core would accept (or the
// other way round) sends the reader to a swap that is refused.

import type { ModrinthVersion } from './api/modrinth';

// Installer type -> the Modrinth loader tags it loads. Quilt loads Fabric mods,
// NeoForge does NOT load Forge mods, Paper runs Spigot and Bukkit plugins.
export const MOD_LOADERS_FOR: Record<string, string[]> = {
    forge: ['forge'],
    neoforge: ['neoforge'],
    fabric: ['fabric'],
    quilt: ['quilt', 'fabric'],
    bukkit: ['bukkit'],
    spigot: ['spigot', 'bukkit'],
    paper: ['paper', 'spigot', 'bukkit'],
    purpur: ['purpur', 'paper', 'spigot', 'bukkit'],
    velocity: ['velocity'],
    waterfall: ['waterfall', 'bungeecord'],
    bungeecord: ['bungeecord'],
};

const PROXIES = new Set(['velocity', 'waterfall', 'bungeecord']);

// Same rule as Core: Bukkit-family plugins hold across the patch releases of
// one Minecraft line (1.21.3 runs on Paper 1.21.4), mods need the exact version.
export const PLUGIN_LOADERS = new Set(['bukkit', 'spigot', 'paper', 'purpur']);

/** First two numbers of a version: 1.21.4 -> 1.21, 26.1.2 -> 26.1. */
function minecraftLine(v: string): string {
    const parts = v.split('.');
    return parts.length < 2 ? v : `${parts[0]}.${parts[1]}`;
}

const LOADER_NAMES: Record<string, string> = {
    forge: 'Forge', neoforge: 'NeoForge', fabric: 'Fabric', quilt: 'Quilt',
    bukkit: 'Bukkit', spigot: 'Spigot', paper: 'Paper', purpur: 'Purpur',
    velocity: 'Velocity', waterfall: 'Waterfall', bungeecord: 'BungeeCord', vanilla: 'Vanilla',
};

export function loaderName(tag: string): string {
    return LOADER_NAMES[tag.toLowerCase()] ?? tag;
}

/** Same rule as Core's ModFitKnown: a known loader, and an MC version unless it is a proxy. */
export function modFitKnown(serverLoader: string, serverMc: string): boolean {
    const loader = serverLoader.trim().toLowerCase();
    if (!MOD_LOADERS_FOR[loader]) return false;
    return PROXIES.has(loader) || serverMc.trim() !== '';
}

/**
 * Why this build does not fit the server, as a short tag ("Forge build on a
 * NeoForge server", "Built for MC 26.3"), or null when it fits or the server
 * does not say enough to tell.
 */
export function buildMismatch(
    serverLoader: string,
    serverMc: string,
    build: Pick<ModrinthVersion, 'loaders' | 'game_versions'>,
): string | null {
    if (!modFitKnown(serverLoader, serverMc)) return null;
    const loader = serverLoader.trim().toLowerCase();
    const accepted = MOD_LOADERS_FOR[loader];
    const loaders = build.loaders.map(l => l.trim().toLowerCase());
    if (!loaders.some(l => accepted.includes(l))) {
        const names = loaders.length ? loaders.map(loaderName).join('/') : 'Unknown';
        return `${names} build on a ${loaderName(loader)} server`;
    }
    if (PROXIES.has(loader)) return null;
    const mc = serverMc.trim();
    const plugin = PLUGIN_LOADERS.has(loader);
    if (build.game_versions.some(g => g.trim() === mc || (plugin && minecraftLine(g.trim()) === minecraftLine(mc)))) return null;
    // Modrinth lists game versions oldest first; the newest is the telling one.
    const newest = build.game_versions[build.game_versions.length - 1];
    return newest ? `Built for MC ${newest}` : 'Built for no listed MC version';
}
