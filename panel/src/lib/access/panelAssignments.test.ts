import { describe, it, expect } from 'vitest';
import { privilegedUsers, searchUsers } from './panelAssignments';
import type { User } from '@/lib/api';
import type { PanelRole, PanelAssignment } from '@/lib/api/panelRoles';

// The Roles screen listed every account in one table and put the LEGACY role in
// the column beside a button that edits the PANEL role. The two are different
// concepts, so the column and the button disagreed about every staff member -
// and there was no way to see who held a panel role without opening people one
// at a time.
//
// These pin the two ways the split list can be wrong: leaving somebody out who
// holds privileges, and putting somebody in who holds none.

function user(id: string, username: string, extra: Partial<User> = {}): User {
    return { id, username, isAdmin: false, ...extra };
}

const ROLES: PanelRole[] = [
    { id: 1, name: 'Support', capabilities: ['tickets.read'], isSystem: false },
    { id: 2, name: 'Billing', capabilities: [], isSystem: false },
];

function assignment(userId: string, extra: Partial<PanelAssignment> = {}): PanelAssignment {
    return { userId, panelRoleId: null, grantCaps: [], denyCaps: [], ...extra };
}

describe('privilegedUsers', () => {
    it('lists somebody with a panel role, by the role name', () => {
        const users = [user('u1', 'ann')];
        const out = privilegedUsers(users, [assignment('u1', { panelRoleId: 1 })], ROLES);
        expect(out).toHaveLength(1);
        expect(out[0].roleName).toBe('Support');
    });

    it('lists somebody who holds only capability overrides', () => {
        // No role, but they can do something a default user cannot. Leaving
        // them out would hide a granted capability entirely.
        const out = privilegedUsers(
            [user('u1', 'ann')],
            [assignment('u1', { grantCaps: ['nodes.read'] })],
            ROLES,
        );
        expect(out).toHaveLength(1);
        expect(out[0].roleName).toBeNull();
        expect(out[0].grantCount).toBe(1);
    });

    it('lists somebody who holds only DENY overrides', () => {
        // A deny is a privilege decision too, and it is the one nobody would
        // think to look for. It is also the state you must be able to find in
        // order to undo it.
        const out = privilegedUsers(
            [user('u1', 'ann')],
            [assignment('u1', { denyCaps: ['servers.delete'] })],
            ROLES,
        );
        expect(out.map(p => p.denyCount)).toEqual([1]);
    });

    it('lists a platform admin who has no panel role at all', () => {
        // The case the whole list is about. Admin SHORT-CIRCUITS every panel
        // capability, so an admin with no assignment row holds strictly more
        // than anyone in this table - and used to be the one person missing
        // from it.
        const out = privilegedUsers([user('u1', 'root', { isAdmin: true })], [], ROLES);
        expect(out).toHaveLength(1);
        expect(out[0].isAdmin).toBe(true);
        expect(out[0].roleName).toBeNull();
    });

    it('counts the legacy role column as admin too', () => {
        // Two records of one fact. A mismatch must resolve towards privileged.
        const out = privilegedUsers([user('u1', 'root', { role: 'admin' })], [], ROLES);
        expect(out).toHaveLength(1);
    });

    it('leaves out a default user', () => {
        expect(privilegedUsers([user('u1', 'bob')], [], ROLES)).toEqual([]);
    });

    it('leaves out somebody whose overrides were cleared', () => {
        // Clearing both lists in the editor writes empty arrays, not a deleted
        // row. A row that exists is not the same as a privilege that exists.
        const out = privilegedUsers([user('u1', 'bob')], [assignment('u1')], ROLES);
        expect(out).toEqual([]);
    });

    it('says a role is unnameable rather than absent', () => {
        // A role deleted between the two requests. "no role" would read as
        // "holds nothing", which is the opposite of the truth.
        const out = privilegedUsers([user('u1', 'ann')], [assignment('u1', { panelRoleId: 9 })], ROLES);
        expect(out[0].roleName).toBe('role #9');
    });

    it('ignores an assignment for a user this list cannot see', () => {
        const out = privilegedUsers([user('u1', 'ann')], [assignment('ghost', { panelRoleId: 1 })], ROLES);
        expect(out).toEqual([]);
    });

    it('is ordered by username, so a poll does not reshuffle it', () => {
        const users = [user('u1', 'zoe', { isAdmin: true }), user('u2', 'ann', { isAdmin: true })];
        expect(privilegedUsers(users, [], ROLES).map(p => p.user.username)).toEqual(['ann', 'zoe']);
    });
});

describe('searchUsers', () => {
    const users = [user('u2', 'zoe', { email: 'zoe@example.com' }), user('u1', 'ann', { email: 'ann@corp.test' })];

    it('shows everyone when nothing is typed', () => {
        expect(searchUsers(users, '').map(u => u.username)).toEqual(['ann', 'zoe']);
        expect(searchUsers(users, '   ').map(u => u.username)).toEqual(['ann', 'zoe']);
    });

    it('matches a username', () => {
        expect(searchUsers(users, 'zo').map(u => u.username)).toEqual(['zoe']);
    });

    it('matches an email, which is how you tell two similar names apart', () => {
        expect(searchUsers(users, 'corp').map(u => u.username)).toEqual(['ann']);
    });

    // Lowercasing only the query passes every test written with lowercase
    // fixtures, and then fails on the first person who capitalised their name.
    // So these fixtures are capitalised - and each carries only ONE of the two
    // fields, because a row with both lets the working half answer for the
    // broken one and the assertion proves nothing.
    it('ignores case in the username, not just in the query', () => {
        expect(searchUsers([user('u3', 'Marta')], 'mart')).toHaveLength(1);
    });

    it('ignores case in the email, not just in the query', () => {
        expect(searchUsers([user('u4', 'x', { email: 'Ann@Example.COM' })], 'example.com')).toHaveLength(1);
    });

    it('does not invent matches', () => {
        expect(searchUsers(users, 'zzz')).toEqual([]);
    });

    it('does not fall over on a user with no email', () => {
        expect(searchUsers([user('u1', 'ann')], 'ann')).toHaveLength(1);
    });
});
