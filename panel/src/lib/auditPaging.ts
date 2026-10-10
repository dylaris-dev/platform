// Stepping through the filtered audit list from the detail popup. The list is
// paged from Core, so the entry after the last loaded one may exist but not be
// here yet: that step asks for the next page instead of stopping.

export type AuditStep =
    | { kind: 'index'; index: number }
    | { kind: 'load' }
    | { kind: 'none' };

export function auditStep(index: number, delta: -1 | 1, loaded: number, total: number): AuditStep {
    const next = index + delta;
    if (next < 0) return { kind: 'none' };
    if (next < loaded) return { kind: 'index', index: next };
    if (next < total) return { kind: 'load' };
    return { kind: 'none' };
}
