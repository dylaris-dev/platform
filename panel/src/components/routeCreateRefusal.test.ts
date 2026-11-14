import { describe, expect, it } from 'vitest';

import { ownDomainsHere, routeCreateRefusal } from './RoutesModal';

// A refused route used to be shown as created whenever the refusal came in
// the session shape ({success:false, message}) rather than with `error`.
describe('routeCreateRefusal', () => {
    it.each([
        // Success: Core's 201 body has a message and no success flag.
        ['created', 201, { message: 'Route created', domain: 'play.example.com' }, null],
        ['created, empty body', 200, {}, null],
        // The session shape: success:false + message, no error.
        ['session-shape refusal', 403, { success: false, message: 'Cross-origin request refused' }, 'Cross-origin request refused'],
        // The gateway shape: error + code.
        ['gateway-shape refusal', 403, { success: false, error: 'Add play.own.net under Own domains first.', code: 'domain_unverified' },
            'Add play.own.net under Own domains first.'],
        // fetchAPI's wrap of a text/plain http.Error.
        ['text/plain refusal', 409, { success: false, error: 'play.example.com is already in use', message: 'play.example.com is already in use' },
            'play.example.com is already in use'],
        // denyJSON: error only, no success flag.
        ['capability refusal', 403, { error: 'Forbidden' }, 'Forbidden'],
        // The gateway-off answer: a bare code in error, the sentence in message.
        ['bare code in error', 503, { success: false, error: 'gateway_disabled', message: 'Gateway routing is disabled.' }, 'Gateway routing is disabled.'],
        // A non-2xx is a failure whatever its body says.
        ['non-2xx with a success-looking body', 500, { message: 'Route created' }, 'Route created'],
        ['non-2xx without text', 502, {}, 'The route could not be created. Try again.'],
        ['non-2xx, no body', 500, null, 'The route could not be created. Try again.'],
        // A coded refusal with an empty text says what the code means.
        ['coded, no text', 403, { success: false, error: '', code: 'custom_domain_cap' }, 'You have reached your limit of routes on your own domains.'],
        ['bare code only', 403, { success: false, error: 'domain_unverified' },
            'This domain is not verified yet. Publish both TXT records shown under Your own domains, then check again.'],
        ['unknown code, no text', 403, { success: false, code: 'brand_new' }, 'The route could not be created. Try again.'],
        // The server's own text always wins over the code.
        ['non-owner refusal', 403, { success: false, error: 'The owner of this server has to add and verify play.own.net under their own domains first.', code: 'domain_unverified' },
            'The owner of this server has to add and verify play.own.net under their own domains first.'],
    ] as const)('%s', (_n, status, body, want) => {
        expect(routeCreateRefusal(status, body)).toBe(want);
    });
});

// Route create checks the server OWNER's claim, so the dialog offers own
// domains only to that owner, or to an admin on a server without one.
describe('ownDomainsHere', () => {
    const me = { id: 'u1', isAdmin: false };
    const admin = { id: 'a1', isAdmin: true };
    it.each([
        ['owner by role', { ownerId: 'u1', role: 'owner' }, me, true],
        ['owner by id, no role', { ownerId: 'u1' }, me, true],
        ['invited collaborator', { ownerId: 'u2', role: 'invited' }, me, false],
        ['inherited access', { ownerId: 'u2', role: 'inherited' }, me, false],
        ['admin on a tenant server', { ownerId: 'u2', role: 'admin' }, admin, false],
        ['admin on a server without owner', { ownerId: '', role: 'admin' }, admin, true],
        ['non-admin on a server without owner', { ownerId: '' }, me, false],
        ['admin owning the server', { ownerId: 'a1', role: 'owner' }, admin, true],
        ['no server', undefined, me, false],
        ['no viewer', { ownerId: 'u1', role: 'owner' }, null, false],
    ] as const)('%s', (_n, server, viewer, want) => {
        expect(ownDomainsHere(server, viewer)).toBe(want);
    });
});
