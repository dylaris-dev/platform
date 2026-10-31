import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/**
 * How one sub-server was installed.
 *
 * Recorded on every install so the setup screen can put the same choices back on
 * screen. Before this existed the panel knew only what the SERVERS row said -
 * installer type, MC version, build - and only ever for whichever sub-server was
 * active, so editing a modpack server to change a JVM flag meant re-picking the
 * modpack from memory.
 */
export interface SubServerInstall {
    subServerName: string;
    /** paper | vanilla | fabric | forge | neoforge | upload | upload-zip | modpack | pack (library on installs from before its removal) */
    installerType: string;
    mcVersion: string;
    buildVersion: string;
    loader?: string;
    /** The Modrinth modpack, when it is one. */
    modrinthProjectId?: string;
    modrinthVersionId?: string;
    modrinthProjectSlug?: string;
    /** The in-house pack + build, when the install came from the pack builder. */
    packId?: number;
    packBuildId?: number;
    installedAt: string;
}

/**
 * Every recorded install for a server.
 *
 * A sub-server installed before this was recorded is simply ABSENT from the
 * list, and that is not the same as an empty install: the caller falls back to
 * the server row rather than showing a blank form. "We never wrote this down"
 * and "it was installed with nothing" are different answers.
 */
export async function getSubServerInstalls(
    serverId: number,
): Promise<{ success: boolean; installs?: SubServerInstall[]; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/installs`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        return handleError(e);
    }
}
