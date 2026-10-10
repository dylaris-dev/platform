import { describe, it, expect } from 'vitest';
import { canManagePlayers, isServerLive, mergeAllPlayers } from './playersView';

describe('isServerLive', () => {
    it('is true only for a running server', () => {
        expect(isServerLive('online')).toBe(true);
        for (const s of ['stopped', 'offline', 'starting', 'stopping', 'installing', 'pending_setup', 'disk_full', '', undefined]) {
            expect(isServerLive(s)).toBe(false);
        }
    });
});

describe('mergeAllPlayers', () => {
    const known = [
        { name: 'zed', uuid: 'u-z' },
        { name: 'Alice', uuid: 'u-a' },
        { name: 'bob', uuid: 'u-b' },
        { name: 'Dave', uuid: 'u-d' },
    ];

    it('puts online players on top, then the rest by name', () => {
        const rows = mergeAllPlayers(known, [{ name: 'zed' }, { name: 'Dave' }]);
        expect(rows.map(r => [r.name, r.online])).toEqual([
            ['Dave', true], ['zed', true], ['Alice', false], ['bob', false],
        ]);
    });

    it('lists each player once, matching names case-insensitively, and keeps the uuid', () => {
        const rows = mergeAllPlayers(known, [{ name: 'ALICE' }, { name: 'alice' }]);
        expect(rows.filter(r => r.name.toLowerCase() === 'alice')).toEqual([
            { name: 'ALICE', uuid: 'u-a', online: true },
        ]);
        expect(rows).toHaveLength(4);
    });

    it('shows someone online who is not in the cache yet', () => {
        const rows = mergeAllPlayers([], [{ name: 'Newbie' }]);
        expect(rows).toEqual([{ name: 'Newbie', uuid: undefined, online: true }]);
    });

    it('with nobody online is the cache sorted by name', () => {
        expect(mergeAllPlayers(known, []).map(r => r.name)).toEqual(['Alice', 'bob', 'Dave', 'zed']);
    });

    it('lists a uuid once, under its first (most recent) name', () => {
        const renamed = [
            { name: 'NewName', uuid: 'u-1' },
            { name: 'OldName', uuid: 'u-1' },
            { name: 'Other', uuid: 'u-2' },
        ];
        expect(mergeAllPlayers(renamed, []).map(r => r.name)).toEqual(['NewName', 'Other']);
        // Online under the new name: the old cache entry does not come back.
        expect(mergeAllPlayers(renamed, [{ name: 'newname' }]).map(r => r.name)).toEqual(['newname', 'Other']);
    });
});

describe('canManagePlayers', () => {
    it('lets owners and admins act without a permissions blob', () => {
        expect(canManagePlayers({ role: 'owner' })).toBe(true);
        expect(canManagePlayers({ role: 'admin' })).toBe(true);
        expect(canManagePlayers({})).toBe(true);
    });

    it('keeps a demo visitor read-only whatever the blob says', () => {
        expect(canManagePlayers({ role: 'demo', permissions: { playersManage: true } as never })).toBe(false);
        expect(canManagePlayers(undefined)).toBe(false);
    });

    it('needs players.manage for a member, players.read is not enough', () => {
        for (const role of ['invited', 'inherited'] as const) {
            expect(canManagePlayers({ role })).toBe(false);
            expect(canManagePlayers({ role, permissions: { players: true } as never })).toBe(false);
            expect(canManagePlayers({ role, permissions: { players: true, playersManage: true } as never })).toBe(true);
        }
    });
});
