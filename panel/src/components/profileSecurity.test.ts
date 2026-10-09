import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { fieldForServerError } from './ProfilePopup';

const SRC = path.resolve(path.dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')), '..');
const read = (p: string) => readFileSync(path.join(SRC, p), 'utf8').replace(/\r\n/g, '\n');

// Core's refusals landed in one banner over the form; each now marks the field
// it is about, and only what concerns no field stays in the banner.
describe('fieldForServerError', () => {
    it.each([
        ['Invalid current password', 'current'],
        ['Invalid 2FA code', 'totp'],
        ['Enter a code from your authenticator to change your email or password', 'totp'],
        ['Password must be at least 12 characters', 'newPassword'],
        ['Invalid Minecraft username: 3-16 characters, letters, digits or _', 'minecraft'],
        ['Username already taken', 'username'],
        ['That email address is already in use', 'email'],
        ['Please wait a minute before changing your address again', 'email'],
        ['Update failed', null],
    ])('%s -> %s', (message, field) => {
        expect(fieldForServerError(message)).toBe(field);
    });
});

describe('the profile form', () => {
    const popup = read('components/ProfilePopup.tsx');

    // Current password first, then the new one, then its confirmation; the
    // confirmation used to appear only once the new one was typed.
    it('asks for the current password before the new one', () => {
        const security = popup.slice(popup.indexOf('{currentView === "security" && ('));
        const order = ['{currentPasswordField}', 'id="profile-new-password"', 'id="profile-confirm-password"']
            .map(m => security.indexOf(m));
        expect(order.every(i => i >= 0)).toBe(true);
        expect([...order].sort((a, b) => a - b)).toEqual(order);
        expect(popup).not.toContain('{newPassword && (');
    });

    // Errors are the panel's own, on the field: no browser bubble from
    // `required`, and the first field in error gets the cursor.
    it('marks fields itself and focuses the first one in error', () => {
        const form = popup.slice(popup.indexOf('const ProfilePopup'), popup.indexOf('function SignOutEverywhereRow'));
        expect(form).toContain('noValidate');
        expect(form).not.toMatch(/\srequired\s/);
        expect(popup).toContain("${errors[key] ? 'input-field-error' : ''}");
        expect(popup).toContain("'aria-invalid': !!errors[key] || undefined");
        expect(popup).toMatch(/const first = FIELD_ORDER\.find\(f => errs\[f\.key\]\);[\s\S]*el\?\.focus\(\);/);
    });

    // A save that changes the address used to close on "Saved!" after 1.5 s and
    // drop Core's answer, which says the new address waits for its link.
    it('keeps the popup open with the answer Core gave', () => {
        const layout = read('app/(authed)/layout.tsx');
        expect(layout).not.toContain("setPopupSuccess('Saved!')");
        expect(popup).toContain("setSuccess(result?.message || 'Profile updated.');");
        expect(popup).toContain('Waiting for confirmation:');
    });

    // The code field exists only while a code is asked for; a refusal about it
    // otherwise marks nothing visible and goes to the banner instead.
    it('sends a code refusal to the banner when no code field is shown', () => {
        expect(popup).toContain("if (field && (field !== 'totp' || needsCode)) {");
    });
});

// cravatar.eu went down and every head with it; heads now come through Core.
describe('player heads', () => {
    it('are loaded from Core, with a fallback', () => {
        for (const f of ['app/(authed)/layout.tsx', 'app/(authed)/servers/[id]/players/page.tsx']) {
            const s = read(f);
            expect(s).not.toContain('cravatar.eu');
            expect(s).toContain('<PlayerHead');
        }
        const head = read('components/PlayerHead.tsx');
        expect(head).toContain('`${API_URL}/avatar/${encodeURIComponent(name)}`');
        expect(head).toContain('onError={() => setFailed(true)}');
    });
});
