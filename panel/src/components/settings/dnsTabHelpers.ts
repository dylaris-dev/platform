// Pure helpers behind Settings > DNS, kept out of the component so the rules
// are unit-testable (the suite has no DOM).

import type { DnsKind, DnsKindField } from '@/lib/api/serverDomains';

export const MIN_INTERVAL_SECONDS = 30;

export function credentialFieldsFor(kinds: DnsKind[], kindId: string): DnsKindField[] {
    return kinds.find(k => k.id === kindId)?.fields ?? [];
}

// Names Core could not remove at the provider when the account was deleted.
// They stay live there until someone deletes them by hand, so the warning
// lists them all and is not dismissed on a timer.
export function orphanMessage(names: string[]): string {
    if (names.length === 0) return '';
    return `The account is deleted, but these records could not be removed at the provider and must be deleted there by hand: ${names.join(', ')}`;
}

export function intervalError(raw: string): string {
    const n = Number(raw);
    if (raw.trim() === '' || !Number.isInteger(n)) return 'Enter a whole number of seconds';
    if (n < MIN_INTERVAL_SECONDS) return `Minimum is ${MIN_INTERVAL_SECONDS} seconds`;
    return '';
}

// The API spells a limit "unlimited" or a number string; a never-saved limit
// reads as unlimited. LimitField speaks null / 0 / n.
export function parseLimit(s: string): number | null {
    const t = s.trim();
    if (t === '' || t === 'unlimited') return null;
    const n = Number(t);
    return Number.isInteger(n) && n >= 0 ? n : null;
}

export function formatLimit(v: number | null): string {
    return v === null ? 'unlimited' : String(v);
}

export type PrincipalKind = 'user' | 'role';

export function principalOf(kind: PrincipalKind, id: string): string {
    return `${kind}:${id}`;
}

export function splitPrincipal(p: string): { kind: PrincipalKind; id: string } | null {
    const m = /^(user|role):(.+)$/.exec(p);
    return m ? { kind: m[1] as PrincipalKind, id: m[2] } : null;
}
