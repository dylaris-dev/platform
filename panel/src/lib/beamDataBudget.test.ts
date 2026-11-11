import { describe, expect, it, vi } from 'vitest';
import {
    BUDGET_BLOCKED_MESSAGE,
    BUDGET_DECLINED_MESSAGE,
    budgetConfirmText,
    budgetDecision,
    budgetErrorMessage,
    budgetPastAllowance,
    createBudgetGate,
    type BeamDataBudget,
} from './beamDataBudget';

const reachedOwner: BeamDataBudget = { budget_gb: 1000, used_gb: 1200, reached: true, can_confirm: true };

describe('budgetDecision', () => {
    it.each([
        ['no budget answer', null, 'pass'],
        ['budget unset', { budget_gb: null, used_gb: 5, reached: false, can_confirm: true }, 'pass'],
        ['below the budget', { budget_gb: 1000, used_gb: 5, reached: false, can_confirm: false }, 'pass'],
        ['reached, owner or admin', reachedOwner, 'confirm'],
        ['reached, member', { ...reachedOwner, can_confirm: false }, 'blocked'],
        ['zero budget, owner', { budget_gb: 0, used_gb: 0, reached: true, can_confirm: true }, 'confirm'],
    ] as const)('%s', (_, b, want) => {
        expect(budgetDecision(b as BeamDataBudget | null)).toBe(want);
    });
});

describe('createBudgetGate', () => {
    it('asks before EVERY transfer while the budget is reached', async () => {
        const confirm = vi.fn().mockResolvedValue(true);
        const fetchBudget = vi.fn().mockResolvedValue(reachedOwner);
        const gate = createBudgetGate({ fetchBudget, confirm, now: () => 0 });
        expect(await gate.check('s')).toEqual({ ok: true, ack: true });
        expect(await gate.check('s')).toEqual({ ok: true, ack: true });
        expect(await gate.check('s')).toEqual({ ok: true, ack: true });
        expect(confirm).toHaveBeenCalledTimes(3);
        // The budget itself is cached for 60 s; the question is not.
        expect(fetchBudget).toHaveBeenCalledTimes(1);
    });

    it('re-reads the budget after 60 s', async () => {
        let t = 0;
        const fetchBudget = vi.fn().mockResolvedValue({ ...reachedOwner, reached: false });
        const gate = createBudgetGate({ fetchBudget, confirm: vi.fn(), now: () => t });
        await gate.check('s');
        t = 59_999;
        await gate.check('s');
        expect(fetchBudget).toHaveBeenCalledTimes(1);
        t = 60_000;
        await gate.check('s');
        expect(fetchBudget).toHaveBeenCalledTimes(2);
    });

    it('a declined dialog stops the transfer', async () => {
        const gate = createBudgetGate({ fetchBudget: async () => reachedOwner, confirm: async () => false });
        expect(await gate.check('s')).toEqual({ ok: false, message: BUDGET_DECLINED_MESSAGE });
    });

    it('a member is blocked without a dialog', async () => {
        const confirm = vi.fn();
        const gate = createBudgetGate({ fetchBudget: async () => ({ ...reachedOwner, can_confirm: false }), confirm });
        expect(await gate.check('s')).toEqual({ ok: false, message: BUDGET_BLOCKED_MESSAGE });
        expect(confirm).not.toHaveBeenCalled();
    });

    it('no budget passes without a dialog or an ack', async () => {
        const confirm = vi.fn();
        const gate = createBudgetGate({ fetchBudget: async () => null, confirm });
        expect(await gate.check('s')).toEqual({ ok: true, ack: false });
        expect(confirm).not.toHaveBeenCalled();
    });

    it('a failed budget read passes, Core still gates the ticket', async () => {
        const gate = createBudgetGate({ fetchBudget: async () => { throw new Error('offline'); }, confirm: vi.fn() });
        expect(await gate.check('s')).toEqual({ ok: true, ack: false });
    });

    it('invalidate forces a fresh read', async () => {
        const fetchBudget = vi.fn().mockResolvedValue(null);
        const gate = createBudgetGate({ fetchBudget, confirm: vi.fn(), now: () => 0 });
        await gate.check('s');
        gate.invalidate('s');
        await gate.check('s');
        expect(fetchBudget).toHaveBeenCalledTimes(2);
    });
});

describe('budgetErrorMessage', () => {
    it('maps the ticket refusal codes and nothing else', () => {
        expect(budgetErrorMessage('ticket: HTTP 403: {"code":"data_budget_reached"}')).toBe(BUDGET_BLOCKED_MESSAGE);
        expect(budgetErrorMessage('ticket: HTTP 403: {"code":"data_budget_ack_required"}')).toMatch(/confirm/);
        expect(budgetErrorMessage('ticket: HTTP 500')).toBeNull();
    });
});

describe('budgetConfirmText', () => {
    it('names the usage and the budget', () => {
        const t = budgetConfirmText(reachedOwner);
        expect(t.message).toContain('1200 GB of your 1000 GB budget');
        expect(t.message).not.toMatch(/—/);
    });
});

describe('budgetPastAllowance', () => {
    it.each([
        [1000, 5000, false],
        [5000, 5000, false],
        [6000, 5000, true],
        [null, 5000, false],
        [6000, null, false],
        [null, null, false],
    ] as const)('budget %s, allowance %s -> %s', (b, a, want) => {
        expect(budgetPastAllowance(b, a)).toBe(want);
    });
});
