import { describe, expect, it } from 'vitest';
import { installMcVersion, installTargetMcVersion, neoForgeMinecraft, prefillSelection, type InstallTargetInput } from './installTarget';

// The Online tab's pickers hold Paper 26.3 in every row: what the owner had
// selected before switching tabs.
const base: InstallTargetInput = {
    tab: 'online',
    isProxy: false,
    selectedMajor: '26.3',
    selectedBuild: '26.3',
    uploadFilePicked: false,
    uploadDetection: undefined,
    uploadInstallsSoftware: false,
    uploadVersionChosen: false,
};

describe('installTargetMcVersion', () => {
    const rows: [string, Partial<InstallTargetInput>, string, boolean][] = [
        ['online uses the pickers', {}, '26.3', false],
        ['online uses a patch build', { selectedMajor: '1.20', selectedBuild: '1.20.6' }, '1.20.6', false],
        // The owner's bug: Paper 26.3 online, then the Upload tab, file picked,
        // nothing detected and no version chosen. Was "26.3 needs Java 25".
        ['upload with software, version not chosen, nothing detected',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: { launchable: false }, uploadInstallsSoftware: true }, '', true],
        ['upload with software, version not chosen, file still being read',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: null, uploadInstallsSoftware: true }, '', false],
        ['upload with software, no file yet',
            { tab: 'upload', uploadInstallsSoftware: true }, '', false],
        ['upload with software and a chosen version uses it',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: { launchable: false }, uploadInstallsSoftware: true, uploadVersionChosen: true, selectedMajor: '1.21.4', selectedBuild: '1.21.4' }, '1.21.4', false],
        ['upload with software, not chosen, detection names a version',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: { launchable: true, mcVersion: '1.20.1' }, uploadInstallsSoftware: true }, '1.20.1', false],
        ['upload kept as it is uses the detection, not a chosen picker',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: { launchable: true, mcVersion: '1.20.1' }, uploadVersionChosen: true }, '1.20.1', false],
        ['upload kept as it is without a version is unknown',
            { tab: 'upload', uploadFilePicked: true, uploadDetection: { launchable: true } }, '', true],
        ['a proxy upload is never unknown',
            { tab: 'upload', isProxy: true, uploadFilePicked: true, uploadDetection: { launchable: true } }, '', false],
        ['modpack uses its selection', { tab: 'modpack', selectionMcVersion: '1.20.1' }, '1.20.1', false],
        ['pack without a selection is empty', { tab: 'pack' }, '', false],
        ['technic uses its selection', { tab: 'technic', selectionMcVersion: '1.12.2' }, '1.12.2', false],
        ['library ignores the pickers', { tab: 'library' }, '', false],
        ['backup ignores the pickers', { tab: 'backup' }, '', false],
    ];
    for (const [name, over, version, unknown] of rows) {
        it(name, () => {
            expect(installTargetMcVersion({ ...base, ...over })).toEqual({ version, unknown });
        });
    }
});

// Same cases as Core's TestNeoForgeMinecraft; Core is the authority.
describe('neoForgeMinecraft', () => {
    const rows: [string, string][] = [
        ['21.1.264', '1.21.1'], ['21.0.5', '1.21'], ['21.10.3', '1.21.10'],
        ['20.4.237', '1.20.4'], ['20.2.86', '1.20.2'], ['26.1.2.5', '26.1.2'],
        ['26.2.0.7', '26.2'], ['26.3.0.89', '26.3'], [' 21.1.264 ', '1.21.1'],
        ['21.1.0-beta', ''], ['26.1.0.0-alpha.4+snapshot-1', ''], ['0.25w14craftmine.3', ''],
        ['0.1.2', ''], ['21.1', ''], ['', ''], ['garbage', ''], ['99999999999.1.2', ''],
    ];
    it.each(rows)('%s -> %s', (loader, want) => {
        expect(neoForgeMinecraft(loader)).toBe(want);
    });
});

describe('installMcVersion', () => {
    it('records the exact version, never the line', () => {
        expect(installMcVersion('forge', '1.21', '1.21.1')).toBe('1.21.1');
        expect(installMcVersion('paper', '1.21', '1.21.4')).toBe('1.21.4');
        expect(installMcVersion('neoforge', '1.21', '21.1.264')).toBe('1.21.1');
    });
    it('keeps a proxy on its line', () => {
        expect(installMcVersion('velocity', '3.4', '3.4.0')).toBe('3.4');
    });
});

describe('prefillSelection', () => {
    const versions = [
        { major: '1.21', build: '1.21.4' },
        { major: '1.21', build: '1.21.1' },
        { major: '1.20', build: '1.20.1' },
    ];
    const neo = [
        { major: '1.21', build: '21.1.264' },
        { major: '1.21', build: '21.0.5' },
    ];
    it('finds an exact recorded version', () => {
        expect(prefillSelection(versions, '1.21.1', '1.21.1')).toEqual({ major: '1.21', build: '1.21.1' });
        expect(prefillSelection(versions, '1.21.1', '')).toEqual({ major: '1.21', build: '1.21.1' });
    });
    it('finds a NeoForge loader recorded as the build', () => {
        expect(prefillSelection(neo, '1.21.1', '21.1.264')).toEqual({ major: '1.21', build: '21.1.264' });
    });
    it('still reads a legacy line', () => {
        expect(prefillSelection(versions, '1.20', '')).toEqual({ major: '1.20', build: '1.20.1' });
    });
    it('offers nothing it cannot place', () => {
        expect(prefillSelection(versions, '', '')).toBeNull();
        expect(prefillSelection(versions, '1.19', '0.16.9')).toBeNull();
    });
});
