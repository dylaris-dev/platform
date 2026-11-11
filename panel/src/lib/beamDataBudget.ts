// The owner's self-set data budget, as the Beam app's file transfers see it.
// Core gates the beam ticket (handlers/beam/beam.go passDataBudget); this is the
// per-TRANSFER half: a ticket serves a whole session, so while the budget is
// reached the page asks before every upload and download, and only a yes makes
// the next ticket request carry the ack.

/** data_budget from GET /api/beam/config?server_uuid=. */
export interface BeamDataBudget {
    budget_gb: number | null;
    used_gb: number;
    reached: boolean;
    /** The caller is the server's owner or a panel admin. */
    can_confirm: boolean;
}

export type BudgetDecision = 'pass' | 'confirm' | 'blocked';

/** Unknown (null) passes: Core still refuses the ticket if it must. */
export function budgetDecision(b: BeamDataBudget | null | undefined): BudgetDecision {
    if (!b || !b.reached) return 'pass';
    return b.can_confirm ? 'confirm' : 'blocked';
}

export const BUDGET_BLOCKED_MESSAGE =
    "The server owner's data budget for this month is used up. Ask the owner to raise it, or to confirm the transfer themselves.";
export const BUDGET_DECLINED_MESSAGE = 'Transfer cancelled: your data budget for this month is used up.';

/** Turns Core's ticket refusal (carried inside the app's error text) into a
 *  sentence; null for any other error. */
export function budgetErrorMessage(raw: string): string | null {
    if (raw.includes('data_budget_reached')) return BUDGET_BLOCKED_MESSAGE;
    if (raw.includes('data_budget_ack_required')) return 'Your data budget for this month is used up. Start the transfer again to confirm it.';
    return null;
}

export const BUDGET_CACHE_MS = 60_000;

export type GateResult = { ok: true; ack: boolean } | { ok: false; message: string };

/**
 * createBudgetGate checks one transfer. The budget is re-read at most every
 * 60 s per server; the confirmation is NEVER cached, so every transfer while
 * the budget is reached asks again.
 */
export function createBudgetGate(deps: {
    fetchBudget: (serverUuid: string) => Promise<BeamDataBudget | null>;
    confirm: (b: BeamDataBudget) => Promise<boolean>;
    now?: () => number;
}) {
    const now = deps.now ?? Date.now;
    const cache = new Map<string, { at: number; b: BeamDataBudget | null }>();

    const read = async (serverUuid: string) => {
        const hit = cache.get(serverUuid);
        if (hit && now() - hit.at < BUDGET_CACHE_MS) return hit.b;
        let b: BeamDataBudget | null = null;
        try {
            b = await deps.fetchBudget(serverUuid);
        } catch {
            b = null;
        }
        cache.set(serverUuid, { at: now(), b });
        return b;
    };

    return {
        async check(serverUuid: string): Promise<GateResult> {
            const b = await read(serverUuid);
            const decision = budgetDecision(b);
            if (decision === 'pass') return { ok: true, ack: false };
            if (decision === 'blocked') return { ok: false, message: BUDGET_BLOCKED_MESSAGE };
            return (await deps.confirm(b!))
                ? { ok: true, ack: true }
                : { ok: false, message: BUDGET_DECLINED_MESSAGE };
        },
        /** Forget a server's budget, e.g. after Core refused a ticket over it. */
        invalidate(serverUuid: string) {
            cache.delete(serverUuid);
        },
    };
}

/** The dialog text, kept here so the test reads what the user reads. */
export function budgetConfirmText(b: BeamDataBudget): { title: string; message: string } {
    const of = b.budget_gb === null ? '' : ` of your ${b.budget_gb} GB budget`;
    return {
        title: 'Data budget reached',
        message: `Beam transfers have used ${b.used_gb} GB${of} this month.\n\nThis transfer uses traffic you set aside for players. Transfer anyway?`,
    };
}

/** Whether a budget reaches past the plan's data allowance, which then slows
 *  transfers first. Without a budget or an allowance there is nothing to say. */
export function budgetPastAllowance(budgetGb: number | null, allowanceGb: number | null): boolean {
    return budgetGb !== null && allowanceGb !== null && budgetGb > allowanceGb;
}
