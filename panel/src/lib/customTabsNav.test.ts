import { describe, it, expect } from 'vitest';
import { navGroupOf, navGroups, navMatches, parseTabParams, tabHref } from './customTabsNav';

const own = { id: 1, name: 'Survival', uuid: 'aaaa-1111', owner: 'bartis', ownerId: 'u-1', role: 'owner' };
const invited = { id: 2, name: 'Friends', uuid: 'bbbb-2222', owner: 'dave', ownerId: 'u-2', role: 'invited' };
const foreign = { id: 3, name: 'Customer', uuid: 'cccc-3333', owner: 'carlo', ownerId: 'u-3', role: 'admin' };
const foreignInvited = { ...foreign, id: 4, invitedHere: true };
const tabs: Record<number, { id: number; name: string }[]> = {
    1: [{ id: 10, name: 'Map' }], 2: [{ id: 20, name: 'Stats' }], 3: [{ id: 30, name: 'Map' }], 4: [{ id: 40, name: 'Map' }],
};
const tabsOf = (id: number) => tabs[id] ?? [];

describe('custom tabs sidebar', () => {
    it('groups servers like the server sidebar', () => {
        expect([own, invited, foreign, foreignInvited].map(navGroupOf)).toEqual(['mine', 'invited', 'admin', 'invited']);
    });

    it('shows other users\' servers only in admin mode', () => {
        const off = navGroups([own, invited, foreign], tabsOf, '', false);
        expect(off.admin).toEqual([]);
        expect(off.mine.map(s => s.id)).toEqual([1]);
        expect(navGroups([own, invited, foreign], tabsOf, '', true).admin.map(s => s.id)).toEqual([3]);
    });

    it('leaves out servers without tabs', () => {
        expect(navGroups([{ ...own, id: 9 }], tabsOf, '', true).mine).toEqual([]);
    });

    it('searches server name, uuid, owner, owner id and tab names', () => {
        expect(navMatches(foreign, tabsOf(3), 'CARLO')).toBe(true);
        expect(navMatches(foreign, tabsOf(3), 'cccc-33')).toBe(true);
        expect(navMatches(foreign, tabsOf(3), 'u-3')).toBe(true);
        expect(navMatches(invited, tabsOf(2), 'stats')).toBe(true);
        expect(navMatches(invited, tabsOf(2), 'nothing')).toBe(false);
        expect(navGroups([own, invited, foreign], tabsOf, 'carlo', true).admin.map(s => s.id)).toEqual([3]);
    });

    it('round-trips a deep link and refuses malformed ones', () => {
        expect(tabHref(3, 30)).toBe('/tabs?server=3&tab=30');
        expect(parseTabParams('3', '30')).toEqual({ serverId: 3, tabId: 30 });
        for (const [s, t] of [[null, '1'], ['1', null], ['x', '1'], ['1', '-2'], ['1.5', '2']] as const) {
            expect(parseTabParams(s, t)).toBeNull();
        }
    });
});
