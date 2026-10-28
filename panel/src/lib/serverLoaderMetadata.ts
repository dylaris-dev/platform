// Pure helpers for the Content tab's optional "declare loader + MC version"
// flow. An imported/uploaded server arrives with a blank MinecraftVersion and
// a non-loader InstallerType (e.g. "upload"), which silently disables the
// Content tab's loader/version auto-filtering (page.tsx defaultLoader /
// defaultMcVersion). isImportedServer flags that case so the page can show an
// optional, dismissible callout recommending the declare flow - it never
// blocks manual/advanced filtering either way.

// The Modrinth loader tags the Content tab filters by. Mirrors core
// pkg/validate.IsModrinthLoader (validate.go) - kept in sync across both.
export const LOADER_OPTIONS = [
    'paper', 'spigot', 'bukkit', 'purpur',
    'fabric', 'forge', 'quilt', 'neoforge',
    'velocity', 'waterfall', 'bungeecord',
] as const;

const KNOWN_LOADERS = new Set<string>(LOADER_OPTIONS);

export function isKnownLoader(loader: string): boolean {
    return KNOWN_LOADERS.has(loader.toLowerCase());
}

// True when the server is missing metadata the Content tab's auto-filtering
// needs: a blank MinecraftVersion, and/or an InstallerType that isn't a
// Modrinth loader tag (e.g. "upload", "upload-zip", "library", or blank).
export function isImportedServer(server: { installerType?: string; minecraftVersion?: string }): boolean {
    const hasVersion = !!server.minecraftVersion?.trim();
    const hasKnownLoader = isKnownLoader(server.installerType || '');
    return !hasVersion || !hasKnownLoader;
}

export const PACK_LOADERS = ['vanilla', 'fabric', 'forge', 'neoforge', 'quilt'] as const;
export type PackLoader = typeof PACK_LOADERS[number];

export function nextPackBuildLabel(existing: string[]): string {
    // A pre-release or build suffix (0.2.0-beta, 1.0.0+fix) still counts, or the suggestion falls below it.
    const versions = existing.map(label => /^(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(label.trim()))
        .filter((match): match is RegExpExecArray => match !== null)
        .map(match => match.slice(1).map(Number))
        .filter(parts => parts.every(Number.isSafeInteger));
    versions.sort((a, b) => b[0] - a[0] || b[1] - a[1] || b[2] - a[2]);
    const latest = versions[0];
    return latest && Number.isSafeInteger(latest[2] + 1) ? `${latest[0]}.${latest[1]}.${latest[2] + 1}` : '0.1.0';
}

export interface PackLoaderVersion {
    version: string;
    stable: boolean;
    recommended?: boolean;
}

/** Preserve a valid selection; otherwise prefer the provider's recommended stable build. */
export function preferredPackLoaderVersion(versions: PackLoaderVersion[], selected = ''): string {
    if (versions.some(v => v.version === selected)) return selected;
    return versions.find(v => v.recommended && v.stable)?.version
        || versions.find(v => v.stable)?.version
        || versions[0]?.version || '';
}

export function packBuildMetadataError(
    input: { minecraft: string; loader: string; loaderVersion: string },
    minecraftVersions: string[],
    loaderVersions: PackLoaderVersion[],
): string | null {
    if (!minecraftVersions.includes(input.minecraft)) return 'Select an available Minecraft version.';
    if (!PACK_LOADERS.some(loader => loader === input.loader)) return 'Select a supported loader.';
    if (input.loader !== 'vanilla' && !loaderVersions.some(v => v.version === input.loaderVersion)) {
        return 'Select a compatible loader version.';
    }
    return null;
}
