import { describe, it, expect } from 'vitest';
import { visibleCategoriesFor, categoryLabel } from './modrinthCategories';
import type { ModrinthCategory } from '@/lib/api/modrinth';

const cat = (name: string, project_type: string): ModrinthCategory =>
    ({ name, project_type, icon: '', header: '' } as ModrinthCategory);

// Shaped after the real /v2/tag/category response: mod / resourcepack / shader /
// modpack / minecraft_java_server exist, "plugin" does not.
const REAL_SHAPE: ModrinthCategory[] = [
    cat('adventure', 'mod'),
    cat('optimization', 'mod'),
    cat('game-mechanics', 'mod'),
    cat('8x', 'resourcepack'),
    cat('16x', 'resourcepack'),
    cat('32x', 'resourcepack'),
    cat('48x', 'resourcepack'),
    cat('64x', 'resourcepack'),
    cat('vanilla-like', 'shader'),
    cat('skyblock', 'minecraft_java_server'),
];

describe('visibleCategoriesFor', () => {
    // The reported bug, exactly: a Paper server showed the resourcepack
    // resolutions, and picking one searched mods for a tag no mod carries.
    it('never shows resourcepack resolutions to a plugin server', () => {
        const names = visibleCategoriesFor(REAL_SHAPE, 'plugin').map(c => c.name);
        for (const res of ['8x', '16x', '32x', '48x', '64x']) {
            expect(names).not.toContain(res);
        }
    });

    it('gives a plugin server the mod taxonomy, because Modrinth has no plugin tags', () => {
        expect(visibleCategoriesFor(REAL_SHAPE, 'plugin').map(c => c.name))
            .toEqual(['adventure', 'optimization', 'game-mechanics']);
    });

    it('gives a mod server the same set', () => {
        expect(visibleCategoriesFor(REAL_SHAPE, 'mod').map(c => c.name))
            .toEqual(['adventure', 'optimization', 'game-mechanics']);
    });

    // The fallback was the whole bug. No match must mean "nothing", so the UI can
    // say so, not "here is every tag on Modrinth".
    it('returns empty rather than falling back to the full list', () => {
        expect(visibleCategoriesFor([cat('8x', 'resourcepack')], 'mod')).toEqual([]);
    });

    it('de-duplicates by name', () => {
        const dup = [cat('adventure', 'mod'), cat('adventure', 'mod')];
        expect(visibleCategoriesFor(dup, 'mod')).toHaveLength(1);
    });

    it('is empty for an empty input', () => {
        expect(visibleCategoriesFor([], 'mod')).toEqual([]);
    });
    it('keeps resource and shader vocabularies separate from mod tags', () => {
        expect(visibleCategoriesFor(REAL_SHAPE, 'resourcepack').map(c => c.name)).toEqual(['8x', '16x', '32x', '48x', '64x']);
        expect(visibleCategoriesFor(REAL_SHAPE, 'shader').map(c => c.name)).toEqual(['vanilla-like']);
    });
});

describe('categoryLabel', () => {
    it('reads the slug as words', () => {
        expect(categoryLabel('game-mechanics')).toBe('game mechanics');
    });
});
