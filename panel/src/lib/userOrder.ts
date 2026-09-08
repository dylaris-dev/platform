import type { User } from '@/lib/api/types';

/**
 * The order every user PICKER uses.
 *
 * Three bands, and the reason for each:
 *
 *  0. The signed-in user. Whoever opens the picker is by far the most likely
 *     answer, and having to search for yourself in your own list is the kind of
 *     small friction that shows up on every single use.
 *  1. Everyone holding a role - admin, support, anything that is not a plain
 *     user. These are the accounts an operator assigns things to on purpose.
 *  2. Everyone else.
 *
 * Alphabetical inside each band, case-insensitively, so the list does not
 * reorder itself when somebody signs up with a capital letter.
 *
 * This is for pickers only. The management tables in Settings sort by their own
 * column and must keep doing so - a table you can sort is not a list you choose
 * from.
 */

/** Whether a user holds a role beyond plain membership. */
export function hasElevatedRole(u: User): boolean {
    // isAdmin as well as role: role is the newer field and an older account can
    // carry the flag without ever having had a role written. Reading only one of
    // the two puts a real admin in the wrong band.
    return !!u.isAdmin || (!!u.role && u.role !== 'user');
}

function band(u: User, currentUserId?: string): number {
    if (currentUserId && u.id === currentUserId) return 0;
    return hasElevatedRole(u) ? 1 : 2;
}

/**
 * Returns a NEW array; the input is left alone because it is React state
 * somewhere up the tree and sorting in place mutates what a previous render
 * already handed out.
 */
export function sortUsersForPicker(users: User[], currentUserId?: string): User[] {
    return [...users].sort((a, b) => {
        const d = band(a, currentUserId) - band(b, currentUserId);
        if (d !== 0) return d;
        return (a.username || '').localeCompare(b.username || '', undefined, { sensitivity: 'base' });
    });
}
