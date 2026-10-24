import { describe, it, expect } from 'vitest';
import { usagePct, trafficSwitch, dataSwitch, backupSwitch, consequenceOf } from './storeAccount';
import type { StoreAccountSummary } from '@/lib/api/store';

const base: StoreAccountSummary = {
    success: true,
    enabled: true,
    reachable: true,
    linked: true,
    subscribed: true,
    stripe: true,
    trafficMeterConfigured: true,
    backupMeterConfigured: true,
    trafficPrice: '€4.50',
    trafficConsentText: 'I agree to be charged €4.50 for every started terabyte...',
    dataConsentText: 'I agree to be charged €4.50 for every started terabyte of data traffic...',
    backupPrice: '€2.00',
    backupConsentText: 'I agree to be charged €2.00 per started 50 GB block...',
};

describe('usagePct', () => {
    it('fills proportionally', () => {
        expect(usagePct(25, 100)).toBe(25);
    });

    it('stops at the track', () => {
        expect(usagePct(300, 100)).toBe(100);
    });

    // A ceiling of zero means they may hold none of this, which is the one case
    // where a full bar says the opposite of the truth: it reads as "you are at
    // your limit" to somebody who never had one.
    it('a ceiling of zero is an empty bar, not a full one', () => {
        expect(usagePct(0, 0)).toBe(0);
        expect(usagePct(5, 0)).toBe(0);
    });

    it('survives a missing number', () => {
        expect(usagePct(NaN, 100)).toBe(0);
        expect(usagePct(10, NaN)).toBe(0);
    });
});

// Three different reasons a switch cannot be moved, and only one of them is
// anybody's fault. Collapsing them into a disabled control with no explanation
// is how a tenant spends an afternoon on the wrong problem.
describe('the billing switches say why they cannot be moved', () => {
    it('is on when consent was given', () => {
        expect(trafficSwitch({ ...base, trafficBillingEnabled: true })).toEqual({ kind: 'on', price: '€4.50' });
        expect(backupSwitch({ ...base, backupBillingEnabled: true })).toEqual({ kind: 'on', price: '€2.00' });
    });

    it('carries the sentence the store records when it is turned on', () => {
        expect(trafficSwitch(base)).toEqual({ kind: 'off', price: '€4.50', consentText: base.trafficConsentText });
    });

    it('cannot be turned on without a price', () => {
        const s = trafficSwitch({ ...base, trafficPrice: undefined });
        expect(s.kind).toBe('unavailable');
        expect(s.kind === 'unavailable' && s.reason).toMatch(/price/);
        // Already on stays on: withdrawing never needs a price.
        expect(backupSwitch({ ...base, backupPrice: undefined, backupBillingEnabled: true }).kind).toBe('on');
    });

    it('is off when it was not', () => {
        expect(trafficSwitch(base).kind).toBe('off');
        expect(backupSwitch(base).kind).toBe('off');
    });

    it('names the missing subscription', () => {
        const s = trafficSwitch({ ...base, subscribed: false });
        expect(s.kind).toBe('unavailable');
        expect(s.kind === 'unavailable' && s.reason).toContain('no active subscription');
    });

    // A grant made in the PANEL creates no store subscription at all, so it
    // reached the branch above and told a tenant whose access works that their
    // subscription was missing. Two different states, and only one of them is
    // something being wrong.
    it('tells a granted tenant that a grant is why, not a missing subscription', () => {
        const s = trafficSwitch({ ...base, subscribed: false, granted: true });
        expect(s.kind).toBe('unavailable');
        expect(s.kind === 'unavailable' && s.reason).toContain('granted by an administrator');
        expect(s.kind === 'unavailable' && s.reason).not.toContain('no active subscription');
        // And it has to say what that MEANS, because a bar reading "0 of 2000
        // GB" beside it otherwise looks like a limit somebody will enforce.
        expect(s.kind === 'unavailable' && s.reason).toContain('Nothing is charged');
    });

    // An admin grant has no Stripe subscription to meter. Offering a toggle that
    // can only fail is worse than saying why it is not there.
    it('names an admin grant as such', () => {
        const s = backupSwitch({ ...base, stripe: false });
        expect(s.kind).toBe('unavailable');
        expect(s.kind === 'unavailable' && s.reason).toContain('administrator');
    });

    it('says an unconfigured meter is not the tenant to fix', () => {
        const s = backupSwitch({ ...base, backupMeterConfigured: false });
        expect(s.kind).toBe('unavailable');
        expect(s.kind === 'unavailable' && s.reason).toContain('Nothing you can do');
    });

    // The two switches must not read each other's state. They are separate
    // consents to separate charges.
    it('the two switches are independent', () => {
        const s = { ...base, trafficBillingEnabled: true, backupBillingEnabled: false };
        expect(trafficSwitch(s).kind).toBe('on');
        expect(backupSwitch(s).kind).toBe('off');
    });
});

// The label under each switch is the consequence, not the setting name.
// "Metered billing: off" tells a tenant nothing about what they are choosing.
describe('consequenceOf', () => {
    it('says what happens when traffic runs out, both ways', () => {
        expect(consequenceOf('traffic', true)).toContain('billed');
        expect(consequenceOf('traffic', false)).toContain('stop being reachable');
        expect(consequenceOf('traffic', false)).toContain('Nothing is charged');
    });

    it('says data traffic is slowed, never stopped', () => {
        expect(consequenceOf('data', false)).toContain('slowed down');
        expect(consequenceOf('data', false)).toContain('nothing is stopped');
        expect(consequenceOf('data', true)).toContain('billed');
    });

    it('says backups are refused rather than deleted', () => {
        const off = consequenceOf('backup', false);
        expect(off).toContain('refused');
        expect(off).toContain('nothing already stored is deleted');
    });
});

// The data switch rides the player meter and price but is its own consent:
// agreeing to one is never agreeing to the other.
describe('dataSwitch', () => {
    it('is its own consent with its own sentence', () => {
        expect(dataSwitch({ ...base, trafficBillingEnabled: true })).toEqual({ kind: 'off', price: '€4.50', consentText: base.dataConsentText });
        expect(dataSwitch({ ...base, dataBillingEnabled: true })).toEqual({ kind: 'on', price: '€4.50' });
    });

    it('follows the traffic meter being configured', () => {
        expect(dataSwitch({ ...base, trafficMeterConfigured: false }).kind).toBe('unavailable');
    });
});
