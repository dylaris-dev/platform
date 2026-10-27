import { describe, expect, it } from 'vitest';
import type { BuildContentEntry } from '@/lib/api/packs';
import { filterPackContent, togglePackEnvironment } from './packContent';

const entries = [
    { id: 1, prettyName: 'Zinc', targetPath: 'mods/zinc.jar', side: 'both' },
    { id: 2, prettyName: 'Apple', targetPath: 'mods/apple.jar', side: 'client' },
    { id: 3, prettyName: 'Server tools', targetPath: 'config/tools.toml', side: 'server', linked: true, modrinthVersionId: 'old', modrinthLatestVersionId: 'new' },
] as BuildContentEntry[];
const name = (entry: BuildContentEntry) => entry.prettyName;
describe('pack content environment controls', () => {
    it('never permits an entry without any environment', () => {
        expect(togglePackEnvironment('client', 'client')).toBeNull();
        expect(togglePackEnvironment('server', 'server')).toBeNull();
        expect(togglePackEnvironment('both', 'client')).toBe('server');
        expect(togglePackEnvironment('both', 'server')).toBe('client');
        expect(togglePackEnvironment('client', 'server')).toBe('both');
        expect(togglePackEnvironment('server', 'client')).toBe('both');
    });
    it('distinguishes client-only from content for both environments', () => {
        expect(filterPackContent(entries, '', 'client', 'name', name).map(e => e.id)).toEqual([2]);
        expect(filterPackContent(entries, '', 'both', 'name', name).map(e => e.id)).toEqual([1]);
        expect(filterPackContent(entries, '', 'server', 'name', name).map(e => e.id)).toEqual([3]);
    });
    it('combines path search with the selected environment', () => {
        expect(filterPackContent(entries, '  TOOLS.TOML ', 'server', 'name', name).map(e => e.id)).toEqual([3]);
        expect(filterPackContent(entries, 'tools.toml', 'client', 'name', name)).toEqual([]);
    });
    it('sorts without mutating the build content list', () => {
        expect(filterPackContent(entries, '', 'all', 'name', name).map(e => e.id)).toEqual([2, 3, 1]);
        expect(filterPackContent(entries, '', 'all', 'name-desc', name).map(e => e.id)).toEqual([1, 3, 2]);
        expect(filterPackContent(entries, '', 'all', 'environment', name).map(e => e.id)).toEqual([3, 1, 2]);
        expect(filterPackContent(entries, '', 'all', 'updates', name).map(e => e.id)).toEqual([3, 2, 1]);
        expect(entries.map(e => e.id)).toEqual([1, 2, 3]);
    });
});
