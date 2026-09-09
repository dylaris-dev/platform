import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { SETTINGS_INDEX, searchSettings, hrefFor } from './settingsIndex';

// The index is a hand-written map from words people type onto places in the
// panel. Everything in it can rot silently: a page renamed, a tab removed, an
// entry pointing at a screen that no longer exists. The failure is a search
// result that navigates somewhere blank, which is worse than no search - the
// operator concludes the setting is gone.
//
// The suite is logic-only (no jsdom, see the note in ModpacksTab.delivery.test),
// so the structural half reads the source.

const SETTINGS_LAYOUT = readFileSync(
    join(__dirname, '..', 'app', '(authed)', 'settings', 'layout.tsx'),
    'utf8',
);

function tabsDeclaredIn(file: string, constName: string): string[] {
    const src = readFileSync(join(__dirname, '..', 'components', 'settings', file), 'utf8');
    const m = src.match(new RegExp(`export const ${constName}[^=]*=\\s*\\[([^\\]]*)\\]`));
    if (!m) throw new Error(`${constName} not found in ${file}`);
    return [...m[1].matchAll(/'([^']+)'/g)].map(x => x[1]);
}

describe('the settings index points at places that exist', () => {
    it('every page slug is a real settings page', () => {
        // The nav is the authority on which pages exist; it is where they are
        // declared and where one would be removed from.
        const declared = new Set(
            [...SETTINGS_LAYOUT.matchAll(/slug:\s*'([a-z0-9-]+)'/g)].map(m => m[1]),
        );
        expect(declared.size).toBeGreaterThan(10);

        const missing = [...new Set(SETTINGS_INDEX.map(e => e.page))].filter(p => !declared.has(p));
        expect(missing, 'index entries pointing at a page the settings nav does not have').toEqual([]);
    });

    it('every tab it names is a tab that page actually has', () => {
        const tabsByPage: Record<string, string[]> = {
            users: tabsDeclaredIn('UserManagementTab.tsx', 'USER_TABS'),
            nodes: tabsDeclaredIn('NodesTab.tsx', 'NODE_TABS'),
            gateway: tabsDeclaredIn('GatewayTab.tsx', 'GATEWAY_TABS'),
            mailing: tabsDeclaredIn('MailingTab.tsx', 'MAILING_TABS'),
        };

        const bad = SETTINGS_INDEX
            .filter(e => e.tab)
            .filter(e => !(tabsByPage[e.page] ?? []).includes(e.tab!))
            .map(e => `${e.page}?tab=${e.tab}`);

        expect(bad, 'index entries naming a tab that page does not render').toEqual([]);
    });

    it('no entry is missing the words a person would type', () => {
        const thin = SETTINGS_INDEX.filter(e => (e.keywords ?? []).length === 0).map(e => e.label);
        expect(thin, 'entries with no keywords match only their own label, which is the one thing the operator does not know').toEqual([]);
    });
});

describe('searchSettings', () => {
    // The point of the index over a filter across visible labels: these words
    // are nowhere on the screens they lead to.
    it.each([
        ['smtp', 'mailing'],
        ['resend', 'mailing'],
        ['2fa', 'users'],
        ['r2', 'storage-connections'],
        ['bucket', 'storage-connections'],
        ['wireguard', 'warp'],
        ['ddos', 'gateway'],
        ['throttle', 'beam'],
        ['gdpr', 'users'],
    ])('%s finds a setting on the %s page', (query, page) => {
        const hits = searchSettings(query);
        expect(hits.length, `"${query}" found nothing`).toBeGreaterThan(0);
        expect(hits.map(h => h.page)).toContain(page);
    });

    // Not fuzzy on purpose: a box that answers "beam" with "backup" because two
    // letters line up teaches the operator not to trust it.
    it('does not invent matches', () => {
        expect(searchSettings('zzzqqq')).toEqual([]);
    });

    it('ignores a query too short to mean anything', () => {
        expect(searchSettings('s')).toEqual([]);
        expect(searchSettings(' ')).toEqual([]);
    });

    it('puts an exact label first', () => {
        const hits = searchSettings('upload limits');
        expect(hits[0].label).toBe('Upload limits');
    });

    it('caps the list so it stays a menu rather than a dump', () => {
        // "s" is too short; "se" matches broadly across labels and keywords.
        expect(searchSettings('se').length).toBeLessThanOrEqual(8);
    });
});

describe('hrefFor', () => {
    it('carries the tab so the search lands on the right one', () => {
        expect(hrefFor({ page: 'mailing', tab: 'delivery', label: 'x', where: 'y' }))
            .toBe('/settings/mailing?tab=delivery');
    });

    it('omits it for a page with no tabs', () => {
        expect(hrefFor({ page: 'regions', label: 'x', where: 'y' })).toBe('/settings/regions');
    });
});
