// Finds the config files of a server's mods and plugins, for the beta config
// editor. Nothing maps a mod to its config, so this is a walk of the two places
// configs live: config/ (mods) and plugins/<Plugin>/ (plugins), filtered by
// extension.

export const CONFIG_EXTENSIONS = ['toml', 'json', 'json5', 'yml', 'yaml', 'properties', 'cfg', 'conf', 'ini', 'snbt'];

// A runaway tree (a plugin that writes thousands of data files) must not turn
// one page view into hundreds of requests.
export const MAX_LISTINGS = 120;
export const MAX_FILES = 500;
const MAX_DEPTH = 4;

export interface ListedEntry { name: string; is_dir: boolean }
export type Lister = (path: string) => Promise<ListedEntry[] | null>;

export function isConfigFile(name: string): boolean {
    const dot = name.lastIndexOf('.');
    return dot > 0 && CONFIG_EXTENSIONS.includes(name.slice(dot + 1).toLowerCase());
}

// Directories that hold data rather than settings: worlds, caches, player
// files. Skipped so the list stays the things a person would edit.
const SKIP_DIRS = new Set(['data', 'cache', 'logs', 'playerdata', 'userdata', 'players', 'backups', 'libraries', 'world']);

/**
 * Walks root/config and root/plugins breadth-first and returns config file
 * paths relative to root, sorted. `truncated` says a limit cut the walk short.
 */
export async function findConfigFiles(root: string, list: Lister): Promise<{ files: string[]; truncated: boolean }> {
    const files: string[] = [];
    let listings = 0;
    let truncated = false;
    const queue: { rel: string; depth: number }[] = [{ rel: 'config', depth: 0 }, { rel: 'plugins', depth: 0 }];
    while (queue.length > 0) {
        if (listings >= MAX_LISTINGS || files.length >= MAX_FILES) { truncated = true; break; }
        const { rel, depth } = queue.shift()!;
        listings++;
        const entries = await list(`${root}/${rel}`);
        if (!entries) continue;
        for (const e of entries) {
            // Hidden entries are the server's own bookkeeping, never a config
            // to edit: Paper keeps plugins/.paper-remapped/**/index.json.
            if (e.name.startsWith('.')) continue;
            const path = `${rel}/${e.name}`;
            if (e.is_dir) {
                if (depth < MAX_DEPTH && !SKIP_DIRS.has(e.name.toLowerCase())) queue.push({ rel: path, depth: depth + 1 });
            } else if (isConfigFile(e.name) && !(rel === 'plugins')) {
                // A file directly in plugins/ is a jar or the server's own
                // bookkeeping, never a plugin's config.
                files.push(path);
            }
        }
    }
    return { files: files.slice(0, MAX_FILES).sort((a, b) => a.localeCompare(b)), truncated };
}
