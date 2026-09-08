import { describe, it, expect } from 'vitest';
import { sortUsersForPicker, hasElevatedRole } from '@/lib/userOrder';
import type { User } from '@/lib/api/types';

const u = (id: string, username: string, extra: Partial<User> = {}): User =>
    ({ id, username, isAdmin: false, ...extra }) as User;

describe('sortUsersForPicker', () => {
    it('puts the signed-in user first, then roles, then everyone else', () => {
        const users = [
            u('3', 'zoe'),
            u('1', 'alice'),
            u('4', 'bob', { role: 'support' }),
            u('2', 'me'),
            u('5', 'carol', { isAdmin: true }),
        ];
        expect(sortUsersForPicker(users, '2').map(x => x.username))
            .toEqual(['me', 'bob', 'carol', 'alice', 'zoe']);
    });

    it('sorts case-insensitively, so a capital letter does not jump the queue', () => {
        const users = [u('1', 'banana'), u('2', 'Apple'), u('3', 'cherry')];
        expect(sortUsersForPicker(users).map(x => x.username))
            .toEqual(['Apple', 'banana', 'cherry']);
    });

    it('does not mutate the array it was given', () => {
        const users = [u('1', 'zoe'), u('2', 'alice')];
        const before = users.map(x => x.username);
        sortUsersForPicker(users);
        expect(users.map(x => x.username)).toEqual(before);
    });

    it('works with no signed-in user, so nobody is promoted', () => {
        const users = [u('1', 'zoe', { isAdmin: true }), u('2', 'alice')];
        expect(sortUsersForPicker(users).map(x => x.username)).toEqual(['zoe', 'alice']);
    });

    it('keeps the signed-in user first even when they hold a role', () => {
        // Otherwise an admin looking at their own picker lands in the role band
        // and has to look for themselves among their colleagues.
        const users = [u('1', 'aaron', { isAdmin: true }), u('2', 'zed', { isAdmin: true })];
        expect(sortUsersForPicker(users, '2').map(x => x.username)).toEqual(['zed', 'aaron']);
    });
});

describe('hasElevatedRole', () => {
    it('reads BOTH isAdmin and role', () => {
        // An older account can carry the flag with no role ever written, and a
        // newer one can carry the role with the flag untouched. Reading one of
        // the two puts a real admin in the wrong band.
        expect(hasElevatedRole(u('1', 'legacy', { isAdmin: true }))).toBe(true);
        expect(hasElevatedRole(u('2', 'modern', { role: 'admin' }))).toBe(true);
        expect(hasElevatedRole(u('3', 'support', { role: 'support' }))).toBe(true);
        expect(hasElevatedRole(u('4', 'plain', { role: 'user' }))).toBe(false);
        expect(hasElevatedRole(u('5', 'bare'))).toBe(false);
    });
});
