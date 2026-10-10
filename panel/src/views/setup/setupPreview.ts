import type { SubServerInstall } from '@/lib/api/subServerInstalls';

/** One labelled line of the read-only "Installed software" block. */
export interface InstalledRow {
    label: string;
    value: string;
    mono?: boolean;
}

export interface SetupPreviewModel {
    /** The sub-server the read-only form describes. */
    subServer: string;
    /** False when it is a sidebar preview of a sub-server that is not running. */
    isActive: boolean;
    /** Empty when nothing is known about the install; the form says so. */
    installed: InstalledRow[];
    /** Its Minecraft version, for the Java recommendation. */
    mcVersion: string;
}

interface PreviewServer {
    activeSubServer?: string;
    installerType?: string;
    minecraftVersion?: string;
    buildNumber?: string;
}

export const formatInstallerType = (type?: string) => {
    if (!type) return '';
    if (type === 'upload' || type === 'upload-zip') return 'Manual Upload';
    if (type === 'library') return 'From Library';
    if (type === 'pack') return 'My modpack';
    return type.charAt(0).toUpperCase() + type.slice(1);
};

/**
 * What the read-only Setup form shows for the active sub-server or a previewed one.
 *
 * Per sub-server, Core only knows the recorded install. The servers row also
 * names an installer and versions, but they describe whichever sub-server is
 * ACTIVE - shown for a preview, they would claim the other one's software. So
 * the row is the fallback for the active sub-server only, and a preview without
 * a record shows nothing rather than the wrong thing.
 */
export function setupPreviewModel(
    server: PreviewServer,
    installs: SubServerInstall[],
    subServers: string[],
    previewSub: string | null,
): SetupPreviewModel {
    const active = server.activeSubServer || '';
    const subServer = previewSub && previewSub !== active && subServers.includes(previewSub) ? previewSub : active;
    const isActive = subServer === active;
    const rec = installs.find(i => i.subServerName === subServer);

    // Same fallback as SetupView's recordedVersions, for the active one only.
    const row: PreviewServer = isActive ? server : {};
    const type = rec?.installerType || row.installerType || '';
    const mcVersion = rec?.mcVersion || row.minecraftVersion || '';
    const build = rec?.buildVersion || row.buildNumber || '';

    const installed: InstalledRow[] = [];
    if (type) installed.push({ label: 'Software', value: formatInstallerType(type) });
    if (rec?.modrinthProjectId) {
        installed.push({ label: 'Modpack', value: rec.modrinthProjectSlug || rec.modrinthProjectId });
        if (rec.modrinthVersionId) installed.push({ label: 'Pack version', value: rec.modrinthVersionId, mono: true });
    }
    if (rec?.packId) installed.push({ label: 'Pack', value: `#${rec.packId}, build ${rec.packBuildId ?? '?'}` });
    if (mcVersion) installed.push({ label: 'Minecraft', value: mcVersion, mono: true });
    // A modpack's build field repeats its version id; the row above says it.
    if (build && build !== rec?.modrinthVersionId) installed.push({ label: 'Build', value: build, mono: true });
    if (rec?.loader) installed.push({ label: 'Loader', value: rec.loader, mono: true });
    if (rec?.installedAt) {
        const d = new Date(rec.installedAt);
        if (!isNaN(d.getTime())) installed.push({ label: 'Installed', value: d.toLocaleString() });
    }

    return { subServer, isActive, installed, mcVersion };
}
