import { describe, expect, it } from 'vitest';
import { installTargetMcVersion, type InstallTargetInput } from './installTarget';

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
