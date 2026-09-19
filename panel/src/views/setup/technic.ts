import type { TechnicVariant } from '@/lib/api/technic';

/** A Technic pack picked in setup. Core resolves the download itself. */
export interface TechnicSelection {
    slug: string;
    displayName: string;
    variant: TechnicVariant;
    /** Only for Solder packs; empty means the pack's recommended build. */
    build?: string;
    mcVersion?: string;
}

export interface TechnicNotice {
    tone: 'info' | 'warning';
    text: string;
    installable: boolean;
}

/** What the operator is told before installing, by what the node will get. */
export function technicNotice(variant: TechnicVariant): TechnicNotice {
    switch (variant) {
        case 'server':
            return { tone: 'info', installable: true, text: 'The pack author provides a server pack. It will be installed.' };
        case 'client-zip':
        case 'client-solder':
            return {
                tone: 'warning',
                installable: true,
                text: 'This pack has no server pack. The client pack will be installed. Client-only mods can stop the server from starting and then have to be removed from the mods folder.',
            };
        default:
            return { tone: 'warning', installable: false, text: 'This pack offers no download that can be installed.' };
    }
}

/** The installer block SetupView sends for a Technic pack. */
export function technicInstaller(sel: TechnicSelection): Record<string, string> {
    const installer: Record<string, string> = { type: 'technic', technicSlug: sel.slug };
    if (sel.build) installer.technicBuild = sel.build;
    if (sel.mcVersion) installer.mcVersion = sel.mcVersion;
    return installer;
}
