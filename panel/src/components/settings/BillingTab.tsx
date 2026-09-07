"use client";

import { CreditCard, Info } from 'lucide-react';
import {
    getBillingSettings, setBillingSettings, BillingSettings,
} from '@/lib/api/billing';
import { useSettingsForm } from '@/lib/useSettingsForm';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import HelpTip from '@/components/ui/HelpTip';

const SPEC_RE = /^\d+[dwm]$/;
const NUM_RE = /^\d+$/;

const DEFAULTS: BillingSettings = {
    gracePeriod: '3d',
    r2Retention: '1w',
    nodeRetention: '2w',
    r2IncludedGb: '50',
    r2BookableGb: '500',
    presignTtlNodeMin: '60',
    presignTtlByonMin: '360',
    paymentUrl: '',
};

export default function BillingTab() {
    // Core answers 503 feature_disabled when BYON is off. Dropping that used to
    // leave the component's OWN hardcoded defaults on screen looking like stored
    // settings, above a Save button whose write would fail the same way. The
    // hook's null snapshot is what keeps them unsaveable.
    const form = useSettingsForm<BillingSettings>({
        load: async () => {
            const res = await getBillingSettings();
            if (!res.success) return null;
            return {
                gracePeriod: res.gracePeriod || DEFAULTS.gracePeriod,
                r2Retention: res.r2Retention || DEFAULTS.r2Retention,
                nodeRetention: res.nodeRetention || DEFAULTS.nodeRetention,
                r2IncludedGb: res.r2IncludedGb || DEFAULTS.r2IncludedGb,
                r2BookableGb: res.r2BookableGb || DEFAULTS.r2BookableGb,
                presignTtlNodeMin: res.presignTtlNodeMin || DEFAULTS.presignTtlNodeMin,
                presignTtlByonMin: res.presignTtlByonMin || DEFAULTS.presignTtlByonMin,
                paymentUrl: res.paymentUrl || '',
            };
        },
        save: async value => {
            const res = await setBillingSettings(value);
            return { ok: res.success, message: res.message };
        },
        successMessage: 'Billing settings saved.',
    });

    const s = form.value ?? DEFAULTS;
    const patch = form.patch;

    const specsValid =
        SPEC_RE.test(s.gracePeriod) &&
        SPEC_RE.test(s.r2Retention) &&
        SPEC_RE.test(s.nodeRetention);
    // An emptied allowance field would be saved as the built-in default rather
    // than as the zero it looks like, so the save is blocked until it says a
    // number. Zero is legal and means none.
    const allowancesValid = NUM_RE.test(s.r2IncludedGb) && NUM_RE.test(s.r2BookableGb);

    const blockedReason = !specsValid
        ? 'Retention values must look like 3d, 2w or 3m'
        : !allowancesValid
            ? 'Included and bookable backup storage must be a whole number of GB'
            : undefined;

    return (
        <SettingsPage
            title="Billing and non-payment"
            icon={CreditCard}
            description="Platform defaults for BYON tenants. Per-user overrides are set from User management."
            width="2xl"
            loading={form.loading}
        >
            <div className="flex items-start gap-2 rounded-md border border-(--base-04) bg-(--base-01) p-3">
                <Info size={15} className="text-(--base-06) shrink-0 mt-0.5" />
                <p className="text-sm text-(--base-07) leading-relaxed">
                    A tenant who does not pay enters a grace period: everything keeps running, a dunning
                    email goes out and a red banner shows in their panel. After grace they are suspended,
                    with servers stopped and data plus backups kept and still viewable. Their R2 backups
                    and node connection are only removed after the retention windows below. User accounts
                    and server metadata are never deleted.
                </p>
            </div>

            <SettingsCard
                title="Non-payment windows"
                form={form}
                saveBlockedReason={blockedReason}
            >
                <SettingsGroup
                    title="Retention"
                    first
                    help={
                        <>
                            <p className="mb-2">
                                Three points on one timeline, all counted from the event before them:
                            </p>
                            <p className="mb-2">
                                payment missed &rarr; <strong>grace period</strong> &rarr; suspended
                                &rarr; <strong>node retention</strong> disconnects their node, and
                                separately <strong>backup retention</strong> deletes their archives.
                            </p>
                            <p>
                                The last one is the only step here that destroys anything, and it is
                                not undoable. Set it longer than you think you need: a tenant who pays
                                on day 29 of a 30-day window keeps everything, and one who pays on day
                                31 has nothing to come back to.
                            </p>
                        </>
                    }
                >
                    <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
                        <SpecField
                            id="billing-grace"
                            label="Grace period"
                            hint="After a missed payment, before suspension."
                            value={s.gracePeriod}
                            onChange={v => patch({ gracePeriod: v })}
                        />
                        <SpecField
                            id="billing-r2-retention"
                            label="R2 backup retention"
                            hint="After suspension, before backups are deleted."
                            value={s.r2Retention}
                            onChange={v => patch({ r2Retention: v })}
                        />
                        <SpecField
                            id="billing-node-retention"
                            label="Node connection retention"
                            hint="After suspension, before the node is disconnected."
                            value={s.nodeRetention}
                            onChange={v => patch({ nodeRetention: v })}
                        />
                    </div>
                </SettingsGroup>

                <SettingsGroup
                    title="Backup storage per purchased node"
                    description="What a node purchase includes, and what a customer may book on top once they have agreed to be charged for it. Route-only locations carry traffic, not servers, so they bring no backup storage. What a customer with no node gets is set under Settings, Backups."
                >
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="billing-r2-included">Included per unit (GB)</label>
                            <input
                                id="billing-r2-included"
                                type="number"
                                min={0}
                                value={s.r2IncludedGb}
                                onChange={e => patch({ r2IncludedGb: e.target.value })}
                                className="input-field w-32"
                            />
                            <p className="text-xs text-(--base-06)">
                                Free with every purchased node, so a tenant holding two nodes gets
                                twice this. Route-only locations bring none - there is no server of
                                theirs here to back up. 0 includes none.
                            </p>
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="billing-r2-bookable">Bookable per unit (GB)</label>
                            <input
                                id="billing-r2-bookable"
                                type="number"
                                min={0}
                                value={s.r2BookableGb}
                                onChange={e => patch({ r2BookableGb: e.target.value })}
                                className="input-field w-32"
                            />
                            <p className="text-xs text-(--base-06)">
                                The most a tenant can add on top with metered storage on, and where
                                it stops. 0 means nothing is for sale on top. Shown to customers
                                before they agree, and they are notified when it changes.
                            </p>
                        </div>
                    </div>
                </SettingsGroup>

                <SettingsGroup
                    title="Presigned URL lifetime"
                    description="How long a backup upload or download link stays valid."
                >
                    <div className="grid grid-cols-2 gap-4">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="billing-ttl-node">Operator nodes (min)</label>
                            <input
                                id="billing-ttl-node"
                                type="number"
                                min={1}
                                value={s.presignTtlNodeMin}
                                onChange={e => patch({ presignTtlNodeMin: e.target.value })}
                                className="input-field w-32"
                            />
                            <p className="text-xs text-(--base-06)">60 is one hour.</p>
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="billing-ttl-byon">BYON nodes (min)</label>
                            <input
                                id="billing-ttl-byon"
                                type="number"
                                min={1}
                                value={s.presignTtlByonMin}
                                onChange={e => patch({ presignTtlByonMin: e.target.value })}
                                className="input-field w-32"
                            />
                            <p className="text-xs text-(--base-06)">Slower uplinks. 360 is six hours.</p>
                        </div>
                    </div>
                </SettingsGroup>

                <SettingsGroup title="Where to pay">
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="billing-payment-url">Payment URL</label>
                        <input
                            id="billing-payment-url"
                            type="url"
                            value={s.paymentUrl}
                            onChange={e => patch({ paymentUrl: e.target.value })}
                            placeholder="https://pay.example.com/..."
                            className="input-field w-full"
                        />
                        <p className="text-xs text-(--base-06)">
                            Where the red banner and the dunning email link. Leave empty to use the
                            in-app billing page.
                        </p>
                    </div>
                </SettingsGroup>
            </SettingsCard>
        </SettingsPage>
    );
}

function SpecField({
    id,
    label,
    hint,
    value,
    onChange,
}: {
    id: string;
    label: string;
    hint: string;
    value: string;
    onChange: (v: string) => void;
}) {
    const valid = SPEC_RE.test(value);
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label" htmlFor={id}>{label}</label>
            <input
                id={id}
                type="text"
                value={value}
                onChange={e => onChange(e.target.value)}
                aria-invalid={!valid}
                className={`input-field w-full ${valid ? '' : 'border-(--error)'}`}
            />
            <p className="text-xs text-(--base-06)">
                {valid ? hint : 'Use a number followed by d, w or m, for example 3d.'}
            </p>
        </div>
    );
}
