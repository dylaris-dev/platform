import { describe, it, expect, vi, afterEach } from 'vitest';
import { API_URL } from './core';
import { backupDownloadUrl } from './types';
import { downloadTicketAttachmentURL } from './tickets';

// Download links must carry NO credential.
//
// They used to, and had to: a plain anchor navigation cannot send an
// Authorization header, so the token rode in the querystring through the
// GET-only fallback AuthMiddleware accepts. That put a live JWT into access
// logs, browser history and the Referer header of whatever the page navigated
// to next.
//
// It is unnecessary now. The session is a same-origin cookie, so the browser
// attaches it to an anchor navigation, a window.open and a fetch alike. This
// file exists to keep it that way: the failure mode of a regression here is
// silent, because putting the token back in the URL WORKS - it just leaks.

afterEach(() => {
    vi.unstubAllGlobals();
});

// A token in storage must change nothing. Older builds left one behind, and a
// helper that reached for it again would reintroduce the leak on exactly the
// installs that upgraded.
function stubLegacyTokenInStorage() {
    vi.stubGlobal('window', {});
    vi.stubGlobal('localStorage', {
        getItem: (k: string) => (k === 'authToken' || k === 'token' ? 'left-over-jwt' : null),
    });
}

describe('download URLs carry no credential', () => {
    const urls: Array<[string, () => string]> = [
        ['backup archive', () => backupDownloadUrl(8)],
        ['ticket attachment', () => downloadTicketAttachmentURL(4, 9)],
    ];

    for (const [name, build] of urls) {
        it(`${name}: no token parameter`, () => {
            const url = build();
            expect(url).not.toContain('token=');
            expect(url).not.toContain('left-over-jwt');
        });

        it(`${name}: unchanged by a token left in storage`, () => {
            const clean = build();
            stubLegacyTokenInStorage();
            expect(build()).toBe(clean);
        });
    }

    it('the backup URL is still the right endpoint', () => {
        expect(backupDownloadUrl(8)).toBe(`${API_URL}/backup-runs/8/download`);
    });
});
