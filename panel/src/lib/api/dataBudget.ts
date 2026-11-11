import { API_URL, getAuthHeader, handleResponse, handleError } from '@/lib/api/core';

/** The caller's own data budget. budget_gb null = no budget; allowance_gb null =
 *  the plan has no data allowance (self-hosted, or not metered). */
export interface MyDataBudget {
    budget_gb: number | null;
    used_gb: number;
    reached: boolean;
    allowance_gb: number | null;
}

export async function getMyDataBudget(): Promise<{ success: boolean; message?: string } & Partial<MyDataBudget>> {
    try {
        const res = await fetch(`${API_URL}/me/data-budget`, { headers: getAuthHeader() });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

export async function setMyDataBudget(budgetGb: number | null): Promise<{ success: boolean; message?: string }> {
    try {
        const res = await fetch(`${API_URL}/me/data-budget`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ budget_gb: budgetGb }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}
