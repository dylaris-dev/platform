import { describe, expect, it } from 'vitest';

import {
    describeClaim, ownDomainRecords, ownDomainSummary, ownDomainCapLine, PROOF_BADGE,
    PROVIDER_UNKNOWN_TEXT, PROVIDER_COOLDOWN_TEXT, providerErrorNote,
} from './CustomDomainsPanel';
import type { CustomDomain, ProviderClaim } from '@/lib/api/customDomains';

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

    // Local mode renders describeClaim exactly as before; pinned as literals.
    it('keeps the local titles', () => {
        expect(describeClaim(pending, []).title).toBe('Waiting for DNS');
        expect(describeClaim({ ...pending, state: 'blocked' }, []).title).toBe('Ownership not proven');
        expect(describeClaim({ ...pending, state: 'permablocked' }, []).title).toBe('Blocked');
        expect(describeClaim(pending, ['route.eu.example.com']).body).toBe(
            'Publish the TXT record below to prove you own this domain. We check it on our own every 30 minutes, or press check now; a domain is never blocked for not having it yet. '
            + 'For players to reach it, add a CNAME to route.eu.example.com, or an A record to one of our edge addresses.',
        );
    });
});

// The managed gateway: two values at one name, and the domain is usable only
// when both sides have proven it.
describe('the two own-domain records', () => {
    const name = '_dylaris-verify.play.own.net';
    const base: CustomDomain = { domain: 'play.own.net', state: 'pending', attempts: 0, txtName: name, txtValue: 'dylaris-verify=l' };
    const prov = (state: ProviderClaim['state'], lapsesAt?: string): ProviderClaim =>
        state === 'missing' ? { state } : { state, txtName: name, txtValue: 'dylaris-verify=p', ...(lapsesAt ? { lapsesAt } : {}) };
    const LAPSE = '2026-11-20T10:00:00Z';

    it('a local claim has one record, a remote one two at the same name', () => {
        expect(ownDomainRecords(base)).toEqual([
            { purpose: 'platform', label: 'This platform', state: 'pending', name, value: 'dylaris-verify=l', lapsesAt: undefined, detail: null },
        ]);
        const two = ownDomainRecords({ ...base, provider: prov('pending') });
        expect(two.map(r => [r.purpose, r.label, r.name, r.value])).toEqual([
            ['platform', 'This platform', name, 'dylaris-verify=l'],
            ['provider', 'Gateway provider', name, 'dylaris-verify=p'],
        ]);
    });

    it('a provider without a claim has no value; one that could not be read is unknown', () => {
        expect(ownDomainRecords({ ...base, provider: prov('missing') })[1]).toMatchObject({
            state: 'missing', value: '', name, detail: 'Not registered at the gateway provider yet. Check now registers it.',
        });
        expect(ownDomainRecords({ ...base, provider: null })[1]).toMatchObject({ state: 'unknown', value: '', name, detail: PROVIDER_UNKNOWN_TEXT });
        // No local token yet: the name still comes from the provider, or is built.
        expect(ownDomainRecords({ ...base, txtName: undefined, txtValue: undefined, provider: null })[0].name).toBe(name);
    });

    // A state outside the closed set must render, as Unknown, and never throw.
    it.each(['suspended', '', 'VERIFIED'])('provider state %j renders as unknown', (state) => {
        const records = ownDomainRecords({ ...base, state: 'verified', provider: { state: state as ProviderClaim['state'] } });
        expect(records[1]).toMatchObject({ state: 'unknown', detail: 'The gateway provider reported a state this panel does not know.' });
        expect(PROOF_BADGE[records[1].state].label).toBe('Unknown');
        expect(ownDomainSummary(records).usable).toBe(false);
    });

    it.each([
        ['both pending', base.state, undefined, prov('pending'), ['pending', 'pending'], false, 'Not usable yet: neither record is verified.'],
        ['local verified only', 'verified', undefined, prov('pending'), ['verified', 'pending'], false, 'Not usable yet: the gateway provider has not verified its record.'],
        ['provider verified only', 'pending', undefined, prov('verified'), ['pending', 'verified'], false, 'Not usable yet: this platform has not verified its record.'],
        ['both verified', 'verified', undefined, prov('verified'), ['verified', 'verified'], true, 'Both records are verified. You can add routes on this domain.'],
        ['provider lapsing', 'verified', undefined, prov('verified', LAPSE), ['verified', 'lapsing'], true,
            'Usable for now, but a TXT record has gone missing. Publish it again before the date shown, or the routes on this domain are removed.'],
        ['local lapsing', 'verified', LAPSE, prov('verified'), ['lapsing', 'verified'], true,
            'Usable for now, but a TXT record has gone missing. Publish it again before the date shown, or the routes on this domain are removed.'],
        ['provider unknown', 'verified', undefined, null, ['verified', 'unknown'], false, "Not usable yet as far as we know: the gateway provider's status is unknown."],
        ['provider missing', 'verified', undefined, prov('missing'), ['verified', 'missing'], false, 'Not usable yet: the gateway provider has not verified its record.'],
        ['provider blocked', 'verified', undefined, prov('blocked'), ['verified', 'blocked'], false, 'Not usable yet: the gateway provider has not verified its record.'],
        ['provider permablocked', 'verified', undefined, prov('permablocked'), ['verified', 'blocked'], false, 'Not usable yet: the gateway provider has not verified its record.'],
    ] as const)('%s', (_n, state, lapsesAt, provider, states, usable, text) => {
        const records = ownDomainRecords({ ...base, state, ...(lapsesAt ? { lapsesAt } : {}), provider });
        expect(records.map(r => r.state)).toEqual(states);
        if (states[1] === 'blocked') expect(records[1].detail).toBe('The gateway provider refuses this domain.');
        expect(ownDomainSummary(records)).toEqual({ usable, text });
    });

    // Distinguishable without colour: every state has its own word.
    it('every state has its own badge text', () => {
        const labels = Object.values(PROOF_BADGE).map(b => b.label);
        expect(new Set(labels).size).toBe(labels.length);
        expect(PROOF_BADGE.lapsing.label).toBe('Needs attention');
    });

    it('states the fixed provider sentences', () => {
        expect(PROVIDER_UNKNOWN_TEXT).toBe('The gateway provider could not be reached. Its status is unknown.');
        expect(PROVIDER_COOLDOWN_TEXT).toBe('The gateway provider can be checked again in a few minutes.');
    });
});

describe('ownDomainCapLine', () => {
    it.each([
        [undefined, null],
        [{ maxCustomDomains: null, used: 2 }, 'Own-domain addresses on this platform: 2, no limit'],
        [{ maxCustomDomains: 0, used: 0 }, 'Own-domain addresses on this platform: none allowed by the plan'],
        [{ maxCustomDomains: 3, used: 1 }, 'Own-domain addresses on this platform: 1 of 3'],
    ])('%j -> %j', (limit, want) => {
        expect(ownDomainCapLine(limit)).toBe(want);
    });
});

// A provider failure during a check or a list read is shown on the provider
// row whatever its state: Core's text, else the sentence for its code.
describe('providerErrorNote', () => {
    it.each([
        [undefined, null],
        [null, null],
        [{ code: 'provider_key_rejected', message: 'The provider rejected the API key.' }, 'The provider rejected the API key.'],
        [{ code: 'provider_unreachable', message: '' }, 'The provider could not be reached. Try again in a minute.'],
        [{ code: 'provider_unreachable' }, 'The provider could not be reached. Try again in a minute.'],
        [{ code: 'brand_new' }, 'Something went wrong talking to the gateway provider. Try again in a moment.'],
        [{}, null],
    ])('%j -> %j', (e, want) => {
        expect(providerErrorNote(e)).toBe(want);
    });
});
