import type { InstalledMod, ServerModpackContent } from './api/modrinth';

/**
 * Whether an installed mod is one of the pack's own members. By project id
 * when both sides know it, and by file name as well: a scanned Technic or
 * upload pack names its jars, and a member identified later keeps that name.
 */
export function isPackMember(
    mod: Pick<InstalledMod, 'modrinthProjectId' | 'fileName'>,
    contents: Pick<ServerModpackContent, 'modrinthProjectId' | 'fileName'>[],
): boolean {
    return contents.some(c =>
        (!!mod.modrinthProjectId && c.modrinthProjectId === mod.modrinthProjectId)
        || (!!mod.fileName && c.fileName === mod.fileName));
}
