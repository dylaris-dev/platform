// Pure logic behind the Players page: when the roster can be asked for at all,
// and how "everyone who ever joined" is merged with who is online right now.

import type { OnlinePlayer } from '@/lib/api/rcon';
import type { KnownPlayer } from '@/lib/api/players';

// Only a running server answers RCON. Everything else (stopped, offline,
// starting, stopping, installing, disk_full, ...) would fail the call, so the
// page must not make it - nor show the failure as an error banner.
export function isServerLive(status?: string): boolean {
    return status === 'online';
}

export interface AllPlayerRow {
    name: string;
    uuid?: string;
    online: boolean;
}

// Online players first (by name), then everyone else in usercache by name.
// Names compare case-insensitively: MC usernames are, and `list` and the cache
// can disagree on case after a rename. Someone online but not in the cache yet
// (just joined, cache not flushed) still shows. A uuid seen once is not
// listed again under another name: after a rename the cache can hold both, and
// MC writes it most recently used first, so the first name is the current one.
export function mergeAllPlayers(known: KnownPlayer[], online: OnlinePlayer[]): AllPlayerRow[] {
    const byName = (a: AllPlayerRow, b: AllPlayerRow) =>
        a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });
    const uuidOf = new Map<string, string | undefined>();
    for (const k of known) {
        if (k?.name) uuidOf.set(k.name.toLowerCase(), k.uuid);
    }
    const onlineKeys = new Set<string>();
    const top: AllPlayerRow[] = [];
    for (const p of online) {
        const key = p.name.toLowerCase();
        if (onlineKeys.has(key)) continue;
        onlineKeys.add(key);
        top.push({ name: p.name, uuid: uuidOf.get(key), online: true });
    }
    const seen = new Set(onlineKeys);
    const seenUuids = new Set(top.map(r => r.uuid).filter(Boolean));
    const rest: AllPlayerRow[] = [];
    for (const k of known) {
        if (!k?.name) continue;
        const key = k.name.toLowerCase();
        if (seen.has(key) || (k.uuid && seenUuids.has(k.uuid))) continue;
        seen.add(key);
        if (k.uuid) seenUuids.add(k.uuid);
        rest.push({ name: k.name, uuid: k.uuid, online: false });
    }
    return [...top.sort(byName), ...rest.sort(byName)];
}
