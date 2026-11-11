'use client';

import { useEffect, useState } from 'react';
import { HardDriveUpload, Loader2, Save } from 'lucide-react';
import { getMyDataBudget, setMyDataBudget, type MyDataBudget } from '@/lib/api/dataBudget';
import { budgetPastAllowance } from '@/lib/beamDataBudget';
import { LimitField } from '@/components/settings/LimitField';
import { SkeletonCard } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';

/**
 * The owner's own data budget: how much of the data traffic beam may use this
 * month before every upload and download asks first. Lets an owner keep part
 * of the plan's traffic for players. It only adds a question - the plan's own
 * limit is never lifted by it, so a budget above that limit changes nothing.
 *
 * Shown on every Core, hosted or self-hosted; the plan allowance line only
 * where there is one.
 */
export default function DataBudgetCard() {
    const [saved, setSaved] = useState<MyDataBudget | null>(null);
    const [draft, setDraft] = useState<number | null>(null);
    const [loadError, setLoadError] = useState('');
    const [saving, setSaving] = useState(false);

    useEffect(() => {
        getMyDataBudget().then(res => {
            if (!res.success) {
                setLoadError(res.message || 'Could not load your data budget.');
                return;
            }
            const b: MyDataBudget = {
                budget_gb: res.budget_gb ?? null,
                used_gb: res.used_gb ?? 0,
                reached: !!res.reached,
                allowance_gb: res.allowance_gb ?? null,
            };
            setSaved(b);
            setDraft(b.budget_gb);
        });
    }, []);

    const save = async () => {
        if (!saved) return;
        setSaving(true);
        const res = await setMyDataBudget(draft);
        setSaving(false);
        if (!res.success) {
            toast(res.message || 'Saving failed', false);
            return;
        }
        setSaved({ ...saved, budget_gb: draft, reached: draft !== null && saved.used_gb >= draft });
        toast('Data budget saved', true);
    };

    return (
        <section className="card p-5" aria-labelledby="data-budget-title">
            <div className="flex items-start gap-2.5 mb-4">
                <HardDriveUpload size={16} className="text-(--base-06) mt-0.5 shrink-0" />
                <div className="min-w-0">
                    <h2 id="data-budget-title" className="text-sm font-medium text-(--base-09)">Your data budget</h2>
                    <p className="text-xs text-(--base-06) mt-0.5">
                        How much data traffic beam file transfers may use this month. Past it, every upload and
                        download asks you to confirm, and people you share a server with cannot transfer.
                        Set 0 to confirm every transfer.
                    </p>
                </div>
            </div>

            {loadError && <p className="text-xs text-(--error-light)" role="alert">{loadError}</p>}
            {!loadError && !saved && <SkeletonCard height="h-16" />}
            {saved && (
                <div className="space-y-3">
                    <div className="flex flex-wrap items-center justify-between gap-3">
                        <label htmlFor="data-budget-gb" className="text-xs text-(--base-08)">Monthly budget</label>
                        <div className="flex items-center gap-3">
                            <LimitField
                                id="data-budget-gb"
                                describedBy="data-budget-usage"
                                unit="GB"
                                unlimitedLabel="No budget"
                                value={draft}
                                onChange={setDraft}
                            />
                            <button
                                type="button"
                                onClick={save}
                                disabled={saving || draft === saved.budget_gb}
                                className="btn btn-primary btn-sm"
                            >
                                {saving ? <Loader2 size={13} className="animate-spin" /> : <Save size={13} />}
                                Save
                            </button>
                        </div>
                    </div>
                    <p id="data-budget-usage" className="text-xs text-(--base-06) tabular-nums">
                        Used this month: <span className={saved.reached ? 'text-(--warning)' : 'text-(--base-08)'}>{saved.used_gb} GB</span>
                        {saved.allowance_gb !== null && <> of the {saved.allowance_gb} GB your plan includes</>}
                        {saved.reached && <span className="ml-2 text-(--warning)">budget reached</span>}
                    </p>
                    {budgetPastAllowance(draft, saved.allowance_gb) && (
                        <p className="text-xs text-(--warning)">Your plan&apos;s traffic limit applies first.</p>
                    )}
                </div>
            )}
        </section>
    );
}
