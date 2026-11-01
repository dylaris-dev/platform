import { describe, expect, it } from 'vitest';
import { isPathChecked, MAX_DOWNLOAD_PATHS, overPathLimit, selectedPaths, toggleSelection, type FileEntry } from '@dylaris/ui-filebrowser';
import { MAX_CAPTURE_PATHS } from './templates';

const dir = (name: string): FileEntry => ({ name, is_dir: true, size: 0 });
const file = (name: string): FileEntry => ({ name, is_dir: false, size: 1 });

// world/ and config/ expanded; world/region expanded too.
const tree: Record<string, FileEntry[]> = {
    '': [dir('world'), dir('config'), file('server.properties'), dir('logs')],
    world: [dir('region'), file('level.dat')],
    'world/region': [file('r.0.0.mca'), file('r.0.1.mca')],
    config: [file('a.toml')],
};

const set = (...p: string[]) => new Set(p);

describe('isPathChecked', () => {
    it('counts a checked ancestor', () => {
        expect(isPathChecked(set('world'), false, 'world/region/r.0.0.mca')).toBe(true);
        expect(isPathChecked(set('world'), false, 'worldedit')).toBe(false);
        expect(isPathChecked(set(), true, 'anything')).toBe(true);
    });
});

describe('toggleSelection', () => {
    // The old picker turned "untick logs" under select-all into "only logs".
    it('unticking one entry under select-all keeps the rest', () => {
        expect(toggleSelection(tree, set(), true, 'logs')).toEqual(set('world', 'config', 'server.properties'));
    });

    it('unticking a nested file splits its checked ancestors', () => {
        expect(toggleSelection(tree, set('world'), false, 'world/region/r.0.0.mca'))
            .toEqual(set('world/level.dat', 'world/region/r.0.1.mca'));
    });

    it('ticking a folder drops its now redundant descendants', () => {
        expect(toggleSelection(tree, set('world/level.dat', 'config/a.toml'), false, 'world'))
            .toEqual(set('world', 'config/a.toml'));
    });

    it('unticking a nested file under select-all splits every level on the way down', () => {
        expect(toggleSelection(tree, set(), true, 'world/region/r.0.0.mca'))
            .toEqual(set('config', 'server.properties', 'logs', 'world/level.dat', 'world/region/r.0.1.mca'));
    });

    it('unticks a checked entry', () => {
        expect(toggleSelection(tree, set('world', 'logs'), false, 'logs')).toEqual(set('world'));
    });
});

describe('selectedPaths', () => {
    it('select-all is the top-level entries', () => {
        expect(selectedPaths(tree, set(), true)).toEqual(['config', 'logs', 'server.properties', 'world']);
    });

    it('drops paths under a checked folder', () => {
        expect(selectedPaths(tree, set('world', 'world/level.dat', 'config/a.toml'), false)).toEqual(['config/a.toml', 'world']);
    });

    // Core refuses an empty path; the picker must never produce one.
    it('never emits an empty path', () => {
        expect(selectedPaths(tree, set('', 'logs'), false)).toEqual(['logs']);
        expect(selectedPaths({}, set(), true)).toEqual([]);
        expect(selectedPaths({ '': [file('')] }, set(), true)).toEqual([]);
    });
});

describe('overPathLimit', () => {
    // A folder of 1000 region files with one unticked becomes 999 paths.
    const region: Record<string, FileEntry[]> = {
        '': [dir('world')],
        world: [dir('region')],
        'world/region': Array.from({ length: 1000 }, (_, i) => file(`r.${i}.mca`)),
    };
    const paths = selectedPaths(region, toggleSelection(region, set(), true, 'world/region/r.0.mca'), false);

    it('flags a split selection past the capture and download limits', () => {
        expect(paths).toHaveLength(999);
        expect(overPathLimit(paths, MAX_CAPTURE_PATHS)).toBe(true);
        expect(overPathLimit(paths, MAX_DOWNLOAD_PATHS)).toBe(true);
    });

    it('allows exactly the limit', () => {
        expect(overPathLimit(paths.slice(0, MAX_CAPTURE_PATHS), MAX_CAPTURE_PATHS)).toBe(false);
        expect(overPathLimit(paths.slice(0, MAX_CAPTURE_PATHS + 1), MAX_CAPTURE_PATHS)).toBe(true);
    });
});
