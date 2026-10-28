import { describe, it, expect } from 'vitest';
import { isImportedServer, isKnownLoader, LOADER_OPTIONS, nextPackBuildLabel, packBuildMetadataError, preferredPackLoaderVersion } from './serverLoaderMetadata';

describe('isKnownLoader', () => {
    it('accepts every declared LOADER_OPTIONS entry', () => {
        for (const l of LOADER_OPTIONS) expect(isKnownLoader(l)).toBe(true);
    });

    it('is case-insensitive', () => {
        expect(isKnownLoader('Paper')).toBe(true);
        expect(isKnownLoader('NEOFORGE')).toBe(true);
    });

    it('rejects install-source-only types that are not Modrinth loader tags', () => {
        expect(isKnownLoader('vanilla')).toBe(false);
        expect(isKnownLoader('upload')).toBe(false);
        expect(isKnownLoader('upload-zip')).toBe(false);
        expect(isKnownLoader('library')).toBe(false);
        expect(isKnownLoader('modpack')).toBe(false);
    });

    it('rejects empty/unknown values', () => {
        expect(isKnownLoader('')).toBe(false);
        expect(isKnownLoader('bogus')).toBe(false);
    });
});

describe('isImportedServer', () => {
    it('flags a server with a blank MinecraftVersion even if installerType looks like a loader', () => {
        expect(isImportedServer({ installerType: 'paper', minecraftVersion: '' })).toBe(true);
        expect(isImportedServer({ installerType: 'paper' })).toBe(true);
    });

    it('flags a server with a non-loader installerType even if a version is present', () => {
        expect(isImportedServer({ installerType: 'upload', minecraftVersion: '1.20.4' })).toBe(true);
    });

    it('does not flag a server with both a known loader and a version', () => {
        expect(isImportedServer({ installerType: 'fabric', minecraftVersion: '1.21' })).toBe(false);
    });

    it('is case-insensitive on installerType and tolerates whitespace-only version', () => {
        expect(isImportedServer({ installerType: 'PAPER', minecraftVersion: '1.20.4' })).toBe(false);
        expect(isImportedServer({ installerType: 'paper', minecraftVersion: '   ' })).toBe(true);
    });
});

describe('pack build metadata', () => {
    it('suggests the next patch without changing a pack to a different release line', () => {
        expect(nextPackBuildLabel([])).toBe('0.1.0');
        expect(nextPackBuildLabel(['1.2.9', '1.2.10', '0.1.99', 'custom'])).toBe('1.2.11');
        expect(nextPackBuildLabel(['0.2.0-studio-ui'])).toBe('0.2.1');
        expect(nextPackBuildLabel(['1.0.0+hotfix', '0.9.0'])).toBe('1.0.1');
    });

    it('prefers the recommended stable loader while preserving an explicitly selected valid version', () => {
        const versions = [{ version: '48.0.1-beta', stable: false }, { version: '47.4.28', stable: true, recommended: true }, { version: '47.3.0', stable: true }];
        expect(preferredPackLoaderVersion(versions)).toBe('47.4.28');
        expect(preferredPackLoaderVersion(versions, '47.3.0')).toBe('47.3.0');
        expect(preferredPackLoaderVersion(versions, 'wrong')).toBe('47.4.28');
    });

    it('rejects stale loader selections and unsupported Minecraft releases before create', () => {
        const input = { minecraft: '1.20.1', loader: 'forge', loaderVersion: '47.4.28' };
        expect(packBuildMetadataError(input, ['1.20.1'], [{ version: '47.4.28', stable: true }])).toBeNull();
        expect(packBuildMetadataError(input, ['1.21.1'], [{ version: '47.4.28', stable: true }])).toContain('Minecraft');
        expect(packBuildMetadataError(input, ['1.20.1'], [{ version: '52.0.0', stable: true }])).toContain('compatible loader');
    });

    it('allows Vanilla without a loader dependency but rejects unknown loader names', () => {
        expect(packBuildMetadataError({ minecraft: '1.20.1', loader: 'vanilla', loaderVersion: '' }, ['1.20.1'], [])).toBeNull();
        expect(packBuildMetadataError({ minecraft: '1.20.1', loader: 'forg', loaderVersion: '47.4.28' }, ['1.20.1'], [])).toContain('supported loader');
    });
});
