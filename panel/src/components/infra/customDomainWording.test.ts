import { describe, expect, it } from 'vitest';

import { describeClaim } from './CustomDomainsPanel';
import type { CustomDomain } from '@/lib/api/customDomains';

const pending: CustomDomain = { domain: 'play.example', state: 'pending', attempts: 0 };

// A claim that is waiting for DNS is the one screen where the panel tells a
// customer to go and DO something in someone else's control panel. Getting the
// wording wrong here costs them the claim: the deadline passes, the route is
// removed, and nothing errored anywhere.
//
// cnameTargetUsage.test.ts already guards the label from being rendered raw -
// that was the first way this went wrong. It reads source, so it cannot see the
// second way: the request that produces the targets failing, leaving an empty
// list that the wording treated as "the operator configured none".
describe('what a pending claim tells the customer to add', () => {
    it('names the single target when there is one', () => {
        const body = describeClaim(pending, ['route.eu.example.com']).body;
        expect(body).toContain('route.eu.example.com');
    });

    it('lists every region when the customer has to choose', () => {
        const body = describeClaim(pending, ['route.eu.example.com', 'route.us.example.com']).body;
        expect(body).toContain('route.eu.example.com');
        expect(body).toContain('route.us.example.com');
    });

    it('says nothing about a CNAME when the operator configured none', () => {
        const body = describeClaim(pending, []).body;
        expect(body).toContain('points at us');
        expect(body).not.toContain('CNAME');
    });

    // The TXT record is the proof; a CNAME alone proves nothing about whose
    // domain it is. Every variant has to ask for it.
    it('always asks for the TXT record', () => {
        for (const [targets, failed] of [[[], false], [[], true], [['route.eu.example.com'], false]] as const) {
            expect(describeClaim(pending, [...targets], failed).body).toContain('TXT record');
        }
    });

    // The distinction this whole test exists for. Same empty list, different
    // cause, and the customer must not be sent to their DNS provider to create
    // a record nobody named.
    it('admits it when the lookup failed instead of implying there is nothing to add', () => {
        const failed = describeClaim(pending, [], true).body;
        const none = describeClaim(pending, [], false).body;
        expect(failed).not.toEqual(none);
        expect(failed).toMatch(/could not load/i);
    });

    // A verified domain is re-checked daily. The customer has to know the
    // record must stay, and when one went missing, by when to restore it.
    it('tells a verified customer to keep the record, and warns when it is gone', () => {
        const healthy = describeClaim({ ...pending, state: 'verified' }, []);
        expect(healthy.body).toContain('Keep the TXT record');
        const failing = describeClaim({ ...pending, state: 'verified', lapsesAt: '2026-10-11T12:00:00Z' }, []);
        expect(failing.title).not.toBe('Verified');
        expect(failing.body).toContain('routes on this domain are removed');
    });

    it('leaves the other states alone', () => {
        expect(describeClaim({ ...pending, state: 'verified' }, [], true).title).toBe('Verified');
        expect(describeClaim({ ...pending, state: 'permablocked' }, [], true).body).toContain('TXT');
    });
});
