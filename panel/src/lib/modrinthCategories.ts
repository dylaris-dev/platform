import type { ModrinthCategory } from '@/lib/api/modrinth';

/**
 * Which Modrinth category tags belong in a server's content browser.
 *
 * Modrinth's /tag/category vocabulary has NO "plugin" project_type - plugins are
 * searched with the same taxonomy as mods. The old filter took project_type
 * === browseProjectType and, finding nothing for a Paper/Spigot server, fell
 * back to the ENTIRE tag list. That is where "8x / 16x / 32x / 48x / 64x" came
 * from: they are resourcepack resolutions, and selecting one searched mods for a
 * category no mod can have, so the results were always empty.
 *
 * So: plugins read the mod taxonomy, and there is no full-list fallback. An
 * empty result is rendered as "no categories" rather than as every category on
 * Modrinth.
 */
export function visibleCategoriesFor(
    categories: ModrinthCategory[],
    browseProjectType: 'mod' | 'plugin' | 'resourcepack' | 'shader' | 'datapack',
): ModrinthCategory[] {
    const wanted = browseProjectType === 'plugin' ? 'mod' : browseProjectType;
    const seen = new Set<string>();
    return categories.filter(c =>
        c.project_type === wanted && (seen.has(c.name) ? false : (seen.add(c.name), true)),
    );
}

/** "game-mechanics" -> "game mechanics". Display only. */
export function categoryLabel(name: string): string {
    return name.replace(/-/g, ' ');
}
