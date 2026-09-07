import { describe, it, expect } from 'vitest';
import { classifyInstallChange, recommendedWipe, WIPE_TOKENS, type NextInstall } from './installWipe';
import type { SubServerInstall } from './api/subServerInstalls';

const installed = (over: Partial<SubServerInstall> = {}): SubServerInstall => ({
    subServerName: 'survival',
    installerType: 'paper',
    mcVersion: '1.20.4',
    buildVersion: '497',
    installedAt: '2026-08-01T00:00:00Z',
    ...over,
});

const next = (over: Partial<NextInstall> = {}): NextInstall => ({
    tab: 'online', software: 'paper', mcVersion: '1.20.4', buildVersion: '497', ...over,
});

describe('classifyInstallChange', () => {
    /**
     * The one that matters most. Changing a JVM flag must not be read as a
     * reinstall - treating it as one is what made a settings edit destructive,
     * and it is the reason an operator could not touch Java without re-picking
     * their modpack from memory.
     */
    it('reads an unchanged install as a runtime change', () => {
        expect(classifyInstallChange(installed(), next())).toBe('runtime');
    });

    it('spots a version change on the online tab', () => {
        expect(classifyInstallChange(installed(), next({ mcVersion: '1.21.1' }))).toBe('version');
        expect(classifyInstallChange(installed(), next({ buildVersion: '512' }))).toBe('version');
    });

    it('spots a software change as an installer change', () => {
        expect(classifyInstallChange(installed(), next({ software: 'fabric' }))).toBe('installer');
    });

    it('leaves an untouched modpack picker alone', () => {
        // No version id means the operator did not open the picker. That is
        // "keep what is installed", not "reinstall the same thing" - and the
        // difference is whether their world survives the save.
        const prev = installed({ installerType: 'modpack', modrinthVersionId: 'v1' });
        expect(classifyInstallChange(prev, { tab: 'modpack' })).toBe('runtime');
        expect(classifyInstallChange(prev, { tab: 'modpack', modrinthVersionId: 'v1' })).toBe('runtime');
    });

    it('spots a modpack version change', () => {
        const prev = installed({ installerType: 'modpack', modrinthVersionId: 'v1' });
        expect(classifyInstallChange(prev, { tab: 'modpack', modrinthVersionId: 'v2' })).toBe('modpack');
    });

    it('spots a pack build change, and leaves an untouched one alone', () => {
        const prev = installed({ installerType: 'pack', packId: 3, packBuildId: 9 });
        expect(classifyInstallChange(prev, { tab: 'pack' })).toBe('runtime');
        expect(classifyInstallChange(prev, { tab: 'pack', packBuildId: 9 })).toBe('runtime');
        expect(classifyInstallChange(prev, { tab: 'pack', packBuildId: 11 })).toBe('modpack');
    });

    it('treats a sub-server with no record as an installer change', () => {
        // Installed before Core recorded any of this. We cannot tell what is on
        // disk, so claiming nothing changed would skip a cleanup that may be
        // exactly what the operator needs.
        expect(classifyInstallChange(undefined, next())).toBe('installer');
    });
    /**
     * A backup import replaces everything in the sub-server, so it must reach the
     * wipe dialog rather than being read as a settings save. There is no version
     * to compare on this tab, which is exactly why the file selection has to
     * carry the signal.
     */
    it('reads a selected archive as a full install', () => {
        expect(classifyInstallChange(
            installed({ installerType: 'backup' }),
            next({ tab: 'backup', software: undefined, backupFileSelected: true }),
        )).toBe('installer');
    });

    it('reads the backup tab with no archive as a runtime change', () => {
        expect(classifyInstallChange(
            installed({ installerType: 'backup' }),
            next({ tab: 'backup', software: undefined }),
        )).toBe('runtime');
    });

    it('reads switching from paper to a backup import as a full install', () => {
        expect(classifyInstallChange(
            installed(),
            next({ tab: 'backup', software: undefined }),
        )).toBe('installer');
    });
});

describe('recommendedWipe', () => {
    it('recommends nothing for a runtime change', () => {
        // Nothing about the install changed, so there is nothing to clear - and
        // a dialog that opens anyway teaches people to click through it.
        expect(recommendedWipe('runtime')).toEqual([]);
        expect(recommendedWipe('none')).toEqual([]);
    });

    it('clears the mod list for a pack update', () => {
        expect(recommendedWipe('modpack')).toEqual(['mods', 'config', 'libraries', 'jars']);
    });

    it('leaves hand-tuned config alone on a version change', () => {
        // The loader changed; the operator's settings did not. Clearing config
        // nobody asked about is the kind of help that loses an evening's work.
        expect(recommendedWipe('version')).not.toContain('config');
        expect(recommendedWipe('version')).toEqual(['libraries', 'versions', 'jars']);
    });

    it('only ever recommends tokens the backend accepts', () => {
        // The vocabulary is shared with platform/core and platform/node. A
        // recommendation they do not know is a 400 at save time.
        for (const change of ['modpack', 'version', 'installer', 'runtime', 'none'] as const) {
            for (const token of recommendedWipe(change)) {
                expect(WIPE_TOKENS).toContain(token);
            }
        }
    });

    it('never offers the world', () => {
        // Not "unchecked by default" - absent. A destructive default has to be
        // impossible to reach by mis-clicking.
        expect(WIPE_TOKENS as readonly string[]).not.toContain('world');
    });
});
