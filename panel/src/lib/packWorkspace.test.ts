import { describe, expect, it } from 'vitest';
import type { PackBuild } from '@/lib/api/packs';
import { currentPackBuild, newestPackBuild, packBuildStatus, packContentGroup } from './packWorkspace';

const build = (id: number, channel = 'draft', createdAt = '2026-10-27T10:00:00Z') => ({ id, channel, createdAt, frozen: false } as PackBuild);
describe('modpack workspace version labels', () => {
    it('keeps the latest release visible even if a newer draft exists', () => {
        const release = build(2, 'release');
        const draft = build(3, 'draft', '2026-10-27T11:00:00Z');
        expect(currentPackBuild([draft, release])?.id).toBe(2);
        expect(packBuildStatus(release, [draft, release])).toBe('Latest release');
        expect(packBuildStatus(draft, [draft, release])).toBe('Draft');
    });
    it('uses the newest build when no release exists without claiming it is live', () => {
        const builds = [build(1), build(2)];
        expect(currentPackBuild(builds)?.id).toBe(2);
        expect(packBuildStatus(builds[1], builds)).toBe('Latest build');
        expect(newestPackBuild([])).toBeUndefined();
    });
    it('prefers creation date over version spelling or list order', () => {
        expect(newestPackBuild([build(50, 'draft', '2026-10-25T10:00:00Z'), build(2)])?.id).toBe(2);
    });
    it('keeps configuration files and shader/resource packs out of the mod count', () => {
        expect(['mod', 'plugin', 'resourcepack', 'shaderpack', 'config'].map(packContentGroup)).toEqual(['mods', 'mods', 'resources', 'resources', 'configs']);
    });
});
