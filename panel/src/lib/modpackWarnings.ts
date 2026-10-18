import type { ModrinthProjectName } from './api/modrinth';

const NAMED = 5;

/**
 * The line a modpack update or rollback adds for mods players run too: a pack
 * is distributed to players, so moving one of those on the server alone leaves
 * every client on the pack's build unable to join. Null when nothing applies.
 */
export function clientSideNotice(projects: Pick<ModrinthProjectName, 'title' | 'client_side'>[], isModpack: boolean): string | null {
    if (!isModpack) return null;
    const names = projects.filter(p => p.client_side === 'required').map(p => p.title);
    if (names.length === 0) return null;
    const shown = names.slice(0, NAMED).join(', ');
    const more = names.length > NAMED ? ` and ${names.length - NAMED} more` : '';
    return `Players need the same version of ${shown}${more} on their own client, or they cannot connect.`;
}
