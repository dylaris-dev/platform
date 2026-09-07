import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';

// Every page under /account must be reachable from the account dropdown.
//
// Two of them were not. /account/solder-keys was linked from NOWHERE, and
// /account/solder-clients only from one sentence on a modpack detail page - so
// the key needed to link this Solder to the Technic Platform sat behind a URL
// an operator had to know already. Nothing failed anywhere: the routes existed,
// the pages rendered, and typecheck and every test were green.
//
// Checked at the source rather than by rendering, because the failure is a link
// that is ABSENT, and a component test can only assert about what it renders.

const ROOT = path.resolve(__dirname, '..');
const ACCOUNT_DIR = path.join(ROOT, 'app', '(authed)', 'account');
const LAYOUT = path.join(ROOT, 'app', '(authed)', 'layout.tsx');

function accountPages(): string[] {
    return fs
        .readdirSync(ACCOUNT_DIR, { withFileTypes: true })
        .filter(e => e.isDirectory() && fs.existsSync(path.join(ACCOUNT_DIR, e.name, 'page.tsx')))
        .map(e => `/account/${e.name}`);
}

describe('account navigation', () => {
    it('finds the pages at all, so an empty sweep cannot pass', () => {
        // The denominator. A wrong path would make the loop below iterate zero
        // times and report success for a check that never ran.
        expect(accountPages().length).toBeGreaterThanOrEqual(5);
    });

    it('links every account page from the account dropdown', () => {
        const layout = fs.readFileSync(LAYOUT, 'utf8');
        const unlinked = accountPages().filter(href => !layout.includes(`"${href}"`));
        expect(unlinked, `account page(s) with no entry point in the account dropdown: ${unlinked.join(', ')}`).toEqual([]);
    });
});
