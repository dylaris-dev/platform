import type { User } from '@/lib/api';
import type { PanelRole, PanelAssignment } from '@/lib/api/panelRoles';

/**
 * Who, on the Roles screen, is more than a default user.
 *
 * The screen used to list EVERY account in one table and show the legacy
 * `role` column beside a button that edits the PANEL role - two different
 * concepts on one line, disagreeing about every staff member. It is now two
 * lists: the people who hold something, and everybody, searchable.
 */

/** One row of the privileged list. */
export interface PrivilegedUser {
    user: User;
    /** Panel role name, or null when they hold only overrides / admin. */
    roleName: string | null;
    grantCount: number;
    denyCount: number;
    /** The legacy admin flag, which short-circuits every panel capability. */
    isAdmin: boolean;
}

function adminish(user: User): boolean {
    // Both are read because they are two records of the same fact and either
    // can be the one that is set: `role` is the column, `isAdmin` is what the
    // session says. Treating a mismatch as "not an admin" would hide exactly
    // the account that needs looking at.
    return !!user.isAdmin || user.role === 'admin';
}

/**
 * privilegedUsers lists everyone the panel has given something a default
 * account does not have: a panel role, a capability override, or admin.
 *
 * Admin is included even though it is not a panel role, because it
 * SHORT-CIRCUITS every panel capability. A privilege list that leaves out the
 * accounts holding every privilege is wrong in the only direction that matters.
 *
 * Sorted by username so the list does not reshuffle when the poll returns.
 */
export function privilegedUsers(
    users: User[],
    assignments: PanelAssignment[],
    roles: PanelRole[],
): PrivilegedUser[] {
    const byUser = new Map(assignments.map(a => [a.userId, a]));
    const roleName = new Map(roles.map(r => [r.id, r.name]));

    const out: PrivilegedUser[] = [];
    for (const user of users) {
        const a = byUser.get(user.id);
        const grantCount = a?.grantCaps.length ?? 0;
        const denyCount = a?.denyCaps.length ?? 0;
        const admin = adminish(user);
        const hasRole = a?.panelRoleId != null;
        if (!hasRole && grantCount === 0 && denyCount === 0 && !admin) continue;

        out.push({
            user,
            // A role id with no matching role is not the same as no role: the
            // row still holds one, we just cannot name it. Say so rather than
            // rendering it as unassigned.
            roleName: hasRole ? (roleName.get(a!.panelRoleId!) ?? `role #${a!.panelRoleId}`) : null,
            grantCount,
            denyCount,
            isAdmin: admin,
        });
    }
    return out.sort((x, y) => x.user.username.localeCompare(y.user.username));
}

/**
 * searchUsers filters the "all users" box.
 *
 * Substring over username and email, not fuzzy - same reasoning as the settings
 * search: a list that answers "bar" with "foo" teaches the reader not to trust
 * it, and this list assigns privileges. An empty query returns everyone,
 * because the box is a browser first and a search second.
 */
export function searchUsers(users: User[], query: string): User[] {
    const q = query.trim().toLowerCase();
    const sorted = [...users].sort((a, b) => a.username.localeCompare(b.username));
    if (!q) return sorted;
    return sorted.filter(u =>
        u.username.toLowerCase().includes(q) || (u.email ?? '').toLowerCase().includes(q));
}
