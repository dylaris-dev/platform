import type { SubServerInstall } from '@/lib/api/subServerInstalls';

/**
 * What may be cleared before an install, and what should be.
 *
 * An install used to write straight on top of whatever was already there. For a
 * jar swap that is fine; for a modpack update it is not - the new pack's mods
 * are ADDED to the old pack's, and a server that boots at all boots with two
 * versions of half its mod list. Operators were told to delete the folders over
 * SFTP by hand.
 *
 * The vocabulary is TOKENS, matching platform/core (installWipeTokens) and
 * platform/node (installer_wipe.go). It deliberately has no entry for the world:
 * a destructive default has to be impossible to reach by mis-clicking, not
 * merely unchecked.
 */
export const WIPE_TOKENS = ['mods', 'config', 'libraries', 'versions', 'jars'] as const;
export type WipeToken = (typeof WIPE_TOKENS)[number];

export const WIPE_LABELS: Record<WipeToken, string> = {
    mods: 'Mods',
    config: 'Pack configuration',
    libraries: 'Loader libraries',
    versions: 'Version cache',
    jars: 'Server jars',
};

export const WIPE_HINTS: Record<WipeToken, string> = {
    mods: 'mods/. The old pack’s mods stay behind otherwise and load alongside the new ones.',
    config: 'config/ and defaultconfigs/. Keep these to preserve hand-tuned settings, clear them if the pack reorganised them.',
    libraries: 'libraries/. The previous loader’s downloads, which a new loader neither uses nor overwrites.',
    versions: 'versions/. A cache some loaders keep; harmless to clear.',
    jars: 'The server jars in the root. The old one stays next to the new one otherwise.',
};

/** What kind of change an operator is about to make. */
export type InstallChange = 'none' | 'runtime' | 'version' | 'modpack' | 'installer';

export interface NextInstall {
    /** The install tab in use: online | library | upload | backup | modpack | pack. */
    tab: string;
    /**
     * An archive is selected on the backup tab. Unlike every other tab there is
     * no version to compare, so this is the only signal that the operator is
     * asking for a fresh import rather than saving unrelated settings.
     */
    backupFileSelected?: boolean;
    /** Server software on the online tab. */
    software?: string;
    mcVersion?: string;
    buildVersion?: string;
    /** Set when a NEW Modrinth version was picked. */
    modrinthVersionId?: string;
    /** Set when a NEW pack build was picked. */
    packBuildId?: number;
}

const tabToInstaller = (tab: string, software?: string): string => {
    if (tab === 'modpack') return 'modpack';
    if (tab === 'pack') return 'pack';
    if (tab === 'library') return 'library';
    if (tab === 'upload') return 'upload';
    if (tab === 'backup') return 'backup';
    return software || '';
};

/**
 * Classifies what is about to change against what is recorded.
 *
 * 'runtime' is the one that matters most: it means nothing about the INSTALL
 * changed, so the files must not be touched at all. Treating it as a reinstall
 * is what made "change a JVM flag" a destructive operation.
 *
 * With no recorded install - anything set up before Core started recording it -
 * the honest answer is 'installer': we cannot tell what is there, so we must not
 * claim nothing changed.
 */
export function classifyInstallChange(prev: SubServerInstall | undefined, next: NextInstall): InstallChange {
    if (!prev) return 'installer';

    const nextType = tabToInstaller(next.tab, next.software);
    if (nextType && nextType !== prev.installerType) return 'installer';

    if (next.tab === 'modpack') {
        // A version id is only present when the operator picked one. Leaving the
        // picker untouched means "keep what is installed", not "reinstall it".
        if (next.modrinthVersionId && next.modrinthVersionId !== prev.modrinthVersionId) return 'modpack';
        return 'runtime';
    }
    if (next.tab === 'pack') {
        if (next.packBuildId && next.packBuildId !== prev.packBuildId) return 'modpack';
        return 'runtime';
    }
    if (next.tab === 'backup') {
        // Re-importing over an existing sub-server replaces everything in it, so
        // a selected archive is always a full install and always gets the wipe
        // dialog. With no archive picked nothing about the install is changing.
        return next.backupFileSelected ? 'installer' : 'runtime';
    }
    if (next.tab === 'library' || next.tab === 'upload') {
        // Both are "point at a file"; there is no version to compare, so a save
        // on these tabs is only ever a runtime change unless the file changed,
        // which the caller signals by switching installer type.
        return 'runtime';
    }

    const mcChanged = !!next.mcVersion && next.mcVersion !== prev.mcVersion;
    const buildChanged = !!next.buildVersion && next.buildVersion !== prev.buildVersion;
    return mcChanged || buildChanged ? 'version' : 'runtime';
}

/**
 * What to tick for the operator, by what they are changing.
 *
 * Recommended, never silent. The dialog shows the boxes and they are free to
 * untick any of them - the point is that the safe answer is the default and the
 * destructive one is a decision, rather than the reverse.
 *
 * Config is NOT recommended on a version change: the loader changed, the pack's
 * settings did not, and clearing hand-tuned config nobody asked about is the
 * kind of help that loses an evening's work.
 */
export function recommendedWipe(change: InstallChange): WipeToken[] {
    switch (change) {
        case 'modpack':
            // A pack update replaces the mod list wholesale, and its config is
            // the pack's rather than the operator's.
            return ['mods', 'config', 'libraries', 'jars'];
        case 'version':
        case 'installer':
            return ['libraries', 'versions', 'jars'];
        default:
            return [];
    }
}

/** One line saying what is about to happen, for the dialog's heading. */
export function changeSummary(change: InstallChange): string {
    switch (change) {
        case 'modpack':
            return 'You are changing the modpack.';
        case 'version':
            return 'You are changing the version.';
        case 'installer':
            return 'You are changing how this server is installed.';
        default:
            return '';
    }
}
