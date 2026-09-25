// Player management. Three routes behind players.read / players.manage, which
// is what lets a moderator do this job WITHOUT rcon.exec (every command there
// is, `stop` and `save-off` included) and WITHOUT files.read (the whole server
// filesystem). The page used to need both.

import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';
import type { RconResponse } from '@/lib/api/rcon';

// One entry in banned-players.json / whitelist.json / ops.json. Core passes
// these through untouched, so the shape is Minecraft's, not ours.
export interface PlayerListEntry {
    uuid?: string;
    name: string;
    reason?: string;
    source?: string;
    created?: string;
    expires?: string;
    level?: number;
    bypassesPlayerLimit?: boolean;
}

export interface PlayerLists {
    success: boolean;
    bans: PlayerListEntry[];
    whitelist: PlayerListEntry[];
    ops: PlayerListEntry[];
    // Names each list that could NOT be read, and why. An unreadable list is
    // not an empty one, and the UI has to be able to say which it is.
    unavailable?: Record<string, string>;
    message?: string;
}

export type PlayerAction =
    | 'kick' | 'ban' | 'unban' | 'op' | 'deop'
    | 'whitelist_add' | 'whitelist_remove' | 'tell';

const EMPTY: PlayerLists = { success: false, bans: [], whitelist: [], ops: [] };

// What Core actually puts on the wire, plus the `status` handleResponse adds.
interface PlayerListsWire {
    success?: boolean;
    status?: number;
    message?: string;
    bans?: PlayerListEntry[];
    whitelist?: PlayerListEntry[];
    ops?: PlayerListEntry[];
    unavailable?: Record<string, string>;
}

export async function getPlayerLists(serverId: number): Promise<PlayerLists> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/players/lists`, { headers: getAuthHeader() });
        const data = (await handleResponse(res)) as PlayerListsWire;
        if (!data.success) {
            return { ...EMPTY, message: data.message || 'Player lists could not be loaded.' };
        }
        return {
            success: true,
            bans: data.bans || [],
            whitelist: data.whitelist || [],
            ops: data.ops || [],
            unavailable: data.unavailable,
        };
    } catch (err) {
        const failed = handleError(err) as { message?: string };
        return { ...EMPTY, message: failed.message };
    }
}

export async function getOnlinePlayers(serverId: number): Promise<RconResponse> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/players/online`, { headers: getAuthHeader() });
        const data = await res.json();
        // data.error first: a refused command now answers with a real status
        // (409 when RCON is off, 502 when the node did not answer) and carries
        // its reason in `error`, the same field a 200 uses. Reading only
        // `message` here would have turned every one of those into the
        // useless "Request failed".
        if (!res.ok) return { success: false, error: data.error || data.message || 'Request failed' };
        return data;
    } catch (err) {
        return { success: false, error: err instanceof Error ? err.message : 'Network error' };
    }
}

// The caller picks an ACTION, never a command - Core owns the mapping, which is
// why players.manage is narrower than "run anything".
export async function playerAction(
    serverId: number,
    action: PlayerAction,
    player: string,
    extra?: { reason?: string; message?: string },
): Promise<RconResponse> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/players/action`, {
            method: 'POST',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ action, player, ...extra }),
        });
        const data = await res.json();
        if (!res.ok) return { success: false, error: data.message || 'Request failed' };
        return data;
    } catch (err) {
        return { success: false, error: err instanceof Error ? err.message : 'Network error' };
    }
}
