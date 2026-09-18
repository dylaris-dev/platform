import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

// A tab's sections unmount when you leave it, and unmounting unregisters them
// from the save bar - so an edit made on one tab vanishes the moment another is
// clicked, silently, taking the bar with it. GuardedTabs asks first.
//
// The failure this test exists for is the NEXT tabbed page: reaching for the
// plain Tabs component is the obvious thing to do, it looks completely fine, and
// the loss only happens to someone who was mid-edit.
//
// The suite is logic-only (no jsdom - see the note in ModpacksTab.delivery.test
// for why a renderer cannot be loaded here), so this reads the source. That is
// also the right level for it: the rule is about which component a page picks.

const SETTINGS_DIR = __dirname;

function settingsSources(): { name: string; src: string }[] {
    return readdirSync(SETTINGS_DIR)
        .filter(f => f.endsWith('.tsx'))
        .map(f => ({ name: f, src: readFileSync(join(SETTINGS_DIR, f), 'utf8') }));
}

describe('tabbed settings pages guard unsaved work', () => {
    it('no settings component renders the unguarded tab bar', () => {
        const offenders = settingsSources()
            // GuardedTabs is the one thing allowed to render Tabs: it wraps it.
            .filter(f => f.name !== 'GuardedTabs.tsx')
            .filter(f => /<Tabs\b/.test(f.src))
            .map(f => f.name);

        expect(
            offenders,
            'these render <Tabs> directly, so switching a tab discards whatever was being edited on it - use GuardedTabs',
        ).toEqual([]);
    });

    it('the tabbed pages use the guarded one', () => {
        for (const name of ['UserManagementTab.tsx', 'NodesTab.tsx']) {
            const src = readFileSync(join(SETTINGS_DIR, name), 'utf8');
            expect(src, `${name} should render GuardedTabs`).toMatch(/<GuardedTabs\b/);
        }
    });

    // The guard is only worth anything if it asks. A GuardedTabs that switched
    // first and prompted afterwards would be strictly worse than no guard: the
    // work is already gone by the time the question appears.
    it('the guard asks before switching, not after', () => {
        const src = readFileSync(join(SETTINGS_DIR, 'GuardedTabs.tsx'), 'utf8');
        const request = src.slice(src.indexOf('const request ='), src.indexOf('const close ='));
        expect(request).toMatch(/registration\?\.dirty/);
        // The dirty branch sets the pending tab and returns; onChange is only
        // reached when nothing is dirty.
        expect(request.indexOf('setPending')).toBeLessThan(request.lastIndexOf('onChange('));
        expect(request).toMatch(/setPending\(id\);\s*\n\s*return;/);
    });
});
