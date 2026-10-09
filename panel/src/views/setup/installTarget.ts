import { effectiveMcVersion } from '@/lib/javaVersion';
import type { UploadDetection } from '@/lib/uploadDetect';

export type InstallTab = 'online' | 'library' | 'upload' | 'backup' | 'modpack' | 'pack' | 'technic';

export interface InstallTargetInput {
    tab: InstallTab;
    isProxy: boolean;
    /** The version pickers. On the upload tab they belong to it only once chosen. */
    selectedMajor: string;
    selectedBuild: string;
    /** The modpack/pack/technic selection's own Minecraft version. */
    selectionMcVersion?: string;
    uploadFilePicked: boolean;
    /** undefined: no file; null: still being read. */
    uploadDetection: UploadDetection | null | undefined;
    /** Picked software is installed over the upload (not its own jar kept). */
    uploadInstallsSoftware: boolean;
    uploadVersionChosen: boolean;
}

/**
 * The Minecraft version the install will run, which picks the Java, and
 * whether a picked upload leaves it unknown.
 *
 * Each tab answers for itself. The version pickers are shared with the Online
 * tab and get a newest version preselected, so on the upload tab they count
 * only once the operator chose (or the detection matched) one; otherwise
 * switching from "Paper 26.3" online to an upload recommended Java 25 for it.
 */
export function installTargetMcVersion(i: InstallTargetInput): { version: string; unknown: boolean } {
    switch (i.tab) {
        case 'online':
            return { version: effectiveMcVersion(i.selectedMajor, i.selectedBuild), unknown: false };
        case 'modpack':
        case 'pack':
        case 'technic':
            return { version: i.selectionMcVersion || '', unknown: false };
        case 'upload': {
            if (i.uploadInstallsSoftware && i.uploadVersionChosen) {
                return { version: effectiveMcVersion(i.selectedMajor, i.selectedBuild), unknown: false };
            }
            const version = i.uploadDetection?.mcVersion || '';
            return { version, unknown: !version && !i.isProxy && i.uploadFilePicked && !!i.uploadDetection };
        }
        default:
            return { version: '', unknown: false };
    }
}
