import type { StoreAccountSummary } from '@/lib/api/store';

// Presentation rules for the store account card. Pure, so the ones that decide
// whether a switch can be moved and what an allowance bar says are testable
// without a storefront.

// usagePct is what the bar fills to.
//
// A ceiling of zero is NOT a full bar. It means the tenant may hold none of this
// at all, and drawing 100% would read as "you are at your limit" to somebody who
// never had one. Anything at or over the ceiling is 100 - a bar that keeps
// growing past its track is a bar that has stopped meaning anything.
export function usagePct(used: number, ceiling: number): number {
    if (!Number.isFinite(used) || !Number.isFinite(ceiling) || ceiling <= 0) return 0;
    return Math.max(0, Math.min(100, Math.round((used / ceiling) * 100)));
}

// price is the per-unit rate the switch charges; consentText the exact sentence
// the store records when it is turned on. Both come from the store, which reads
// the price from Stripe, so the screen shows what the invoice will use.
export type SwitchState =
    | { kind: 'on'; price?: string }
    | { kind: 'off'; price: string; consentText: string }
    | { kind: 'unavailable'; reason: string };

// trafficSwitch and backupSwitch decide whether the tenant may move the control
// at all, and say why when they may not.
//
// Three different "no"s, and collapsing them is how somebody spends an afternoon
// on the wrong problem: nothing to bill against, no meter configured by the
// operator, or a subscription that is not billed through Stripe at all. Only the
// second is anybody's fault, and it is not the tenant's.
function billingSwitch(
    s: StoreAccountSummary,
    enabled: boolean | undefined,
    meterConfigured: boolean | undefined,
    what: string,
    price: string | undefined,
    consentText: string | undefined,
): SwitchState {
    if (!s.subscribed) {
        // A grant is not a missing subscription, and saying so was actively
        // misleading: a tenant whose BYON works read "there is no active
        // subscription" as though their access were broken. A grant made in the
        // panel writes Core's billing row and creates no store subscription at
        // all, so there is nothing to meter - and nothing sweeps them either,
        // because the guard only walks Stripe subscriptions.
        if (s.granted) {
            return {
                kind: 'unavailable',
                reason: `Your access was granted by an administrator rather than bought, so there is no subscription to meter ${what} against. Nothing is charged, and nothing is stopped for going over.`,
            };
        }
        return { kind: 'unavailable', reason: `There is no active subscription to bill ${what} against.` };
    }
    if (!s.stripe) {
        return {
            kind: 'unavailable',
            reason: 'This subscription was granted by an administrator rather than billed through the store, so there is nothing to meter.',
        };
    }
    if (!meterConfigured) {
        return {
            kind: 'unavailable',
            reason: `Metered ${what} is not set up on the store yet. Nothing you can do from here.`,
        };
    }
    if (enabled) return { kind: 'on', price };
    // A charge is only agreed to with its price in front of the tenant. The
    // store refuses the switch without one, so the screen says why up front.
    if (!price || !consentText) {
        return {
            kind: 'unavailable',
            reason: `The price for metered ${what} could not be read from the store right now, so it cannot be switched on. Try again later.`,
        };
    }
    return { kind: 'off', price, consentText };
}

export function trafficSwitch(s: StoreAccountSummary): SwitchState {
    return billingSwitch(s, s.trafficBillingEnabled, s.trafficMeterConfigured, 'traffic', s.trafficPrice, s.trafficConsentText);
}

// Same meter and price as player traffic, so it is configured exactly when
// that one is.
export function dataSwitch(s: StoreAccountSummary): SwitchState {
    return billingSwitch(s, s.dataBillingEnabled, s.trafficMeterConfigured, 'data traffic', s.trafficPrice, s.dataConsentText);
}

export function backupSwitch(s: StoreAccountSummary): SwitchState {
    return billingSwitch(s, s.backupBillingEnabled, s.backupMeterConfigured, 'backup storage', s.backupPrice, s.backupConsentText);
}

// consequenceOf is the sentence under each switch: what happens when the
// allowance runs out, in the state the switch is currently in.
//
// Written as a consequence rather than as a setting name because that is the
// decision being made. "Metered billing: off" tells a tenant nothing; "you will
// be stopped rather than charged" tells them exactly what they are choosing.
export function consequenceOf(kind: 'traffic' | 'data' | 'backup', on: boolean): string {
    if (kind === 'data') {
        return on
            ? 'Past your included data traffic beam transfers and custom tabs keep full speed and the extra is billed by the terabyte.'
            : 'Past your included data traffic beam transfers and custom tabs are slowed down until the month resets. Nothing is charged, nothing is stopped.';
    }
    if (kind === 'traffic') {
        return on
            ? 'Past your included traffic you keep running and the extra is billed by the terabyte.'
            : 'Past your included traffic your servers stop being reachable until the next period. Nothing is charged.';
    }
    return on
        ? 'Past your included backup storage new backups keep running and the extra is billed per block.'
        : 'Past your included backup storage new backups are refused. Nothing is charged, and nothing already stored is deleted.';
}
