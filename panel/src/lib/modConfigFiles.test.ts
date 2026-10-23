import { describe, expect, it } from 'vitest';
import { findConfigFiles, isConfigFile, MAX_LISTINGS, type ListedEntry } from './modConfigFiles';

const tree: Record<string, ListedEntry[]> = {
    'main/config': [
        { name: 'sodium-options.json', is_dir: false },
        { name: 'create', is_dir: true },
        { name: 'notes.md', is_dir: false },
    ],
    'main/config/create': [{ name: 'server.toml', is_dir: false }],
    'main/plugins': [
        { name: 'EssentialsX.jar', is_dir: false },
        { name: 'bStats', is_dir: true },
        { name: 'Essentials', is_dir: true },
    ],
    'main/plugins/bStats': [{ name: 'config.yml', is_dir: false }],
    'main/plugins/Essentials': [
        { name: 'config.yml', is_dir: false },
        { name: 'userdata', is_dir: true },
    ],
    'main/plugins/Essentials/userdata': [{ name: 'abc.yml', is_dir: false }],
};
const lister = async (p: string) => tree[p] ?? null;

describe('findConfigFiles', () => {
    it('lists mod and plugin configs, skips data dirs, jars and non-config files', async () => {
        const { files, truncated } = await findConfigFiles('main', lister);
        expect(files).toEqual([
            'config/create/server.toml',
            'config/sodium-options.json',
            'plugins/bStats/config.yml',
            'plugins/Essentials/config.yml',
        ].sort((a, b) => a.localeCompare(b)));
        expect(truncated).toBe(false);
    });

    it('stops at the listing budget on a runaway tree and says so', async () => {
        let calls = 0;
        // Ten subdirectories everywhere: the walk would go on for 10^4 listings.
        const endless = async (): Promise<ListedEntry[]> => {
            calls++;
            return Array.from({ length: 10 }, (_, i) => ({ name: `d${i}`, is_dir: true }));
        };
        const { truncated } = await findConfigFiles('main', endless);
        expect(truncated).toBe(true);
        expect(calls).toBeLessThanOrEqual(MAX_LISTINGS);
    });

    it('a missing directory is not an error', async () => {
        expect(await findConfigFiles('none', lister)).toEqual({ files: [], truncated: false });
    });
});

describe('isConfigFile', () => {
    it('matches by extension, case-insensitively', () => {
        expect(isConfigFile('A.TOML')).toBe(true);
        expect(isConfigFile('mod.jar')).toBe(false);
        expect(isConfigFile('.json')).toBe(false);
    });
});
