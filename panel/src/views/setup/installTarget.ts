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

/**
 * The exact Minecraft version a NeoForge build targets, '' when it cannot
 * tell. Mirrors Core's neoForgeMinecraft, which is the authority: "21.1.264"
 * is 1.21.1, "21.0.5" is 1.21, "26.1.2.5" is 26.1.2, "26.3.0.89" is 26.3.
 */
export function neoForgeMinecraft(loader: string): string {
    const v = loader.trim();
    if (/[-+]/.test(v)) return '';
    const parts = v.split('.');
    if (parts.length < 3 || !parts.every(p => /^[0-9]{1,9}$/.test(p))) return '';
    const n = parts.map(Number);
    if (n[0] === 0) return '';
    if (n[0] >= 26) return parts[0] + '.' + parts[1] + (parts.length >= 4 && n[2] !== 0 ? '.' + parts[2] : '');
    return '1.' + parts[0] + (n[1] !== 0 ? '.' + parts[1] : '');
}

/**
 * The Minecraft version an online install records: the picked build for
 * Paper, vanilla, Fabric and Forge (their builds ARE Minecraft versions), the
 * derived one for NeoForge, the line for anything else. The line alone made
 * mods resolve against 1.21 on a 1.21.1 server.
 */
export function installMcVersion(software: string, selectedMajor: string, selectedBuild: string): string {
    if (software === 'neoforge') return neoForgeMinecraft(selectedBuild);
    return EXACT_BUILD_SOFTWARE.includes(software) ? selectedBuild : selectedMajor;
}

/** Software whose picker build is the exact Minecraft version. A proxy's is its own release. */
const EXACT_BUILD_SOFTWARE = ['paper', 'vanilla', 'fabric', 'forge'];

/**
 * Which picker entry a recorded install puts back on the form. The recorded
 * build finds it first (an exact Minecraft version, or a NeoForge loader);
 * then the recorded Minecraft version as a build; then, for records from
 * before Core stored exact versions, the Minecraft version as a line.
 */
export function prefillSelection(
    versions: { major: string; build: string }[],
    mcVersion: string,
    buildVersion: string,
): { major: string; build: string } | null {
    const byBuild = (b: string) => (b ? versions.find(v => v.build === b) : undefined);
    const hit = byBuild(buildVersion) || byBuild(mcVersion);
    if (hit) return { major: hit.major, build: hit.build };
    if (!mcVersion) return null;
    const inLine = versions.filter(v => v.major === mcVersion);
    if (inLine.length === 0) return null;
    return { major: mcVersion, build: inLine[0].build };
}
