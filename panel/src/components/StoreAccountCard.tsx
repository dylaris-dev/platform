"use client";

import { useState } from 'react';
import { CloudOff } from 'lucide-react';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import ConsentRow from '@/components/billing/ConsentRow';
import { usagePct, backupSwitch } from '@/lib/storeAccount';
import { setStoreBillingConsent, type StoreAccountSummary } from '@/lib/api/store';

// What the tenant bought and how much of it is left, on the panel side.
//
// The numbers are the store's - it owns the money and already computes them for
// its own screens - and travel through Core, which names the account from the
// session. Nothing here recomputes an allowance: a second copy is the one that
// drifts, and this platform has already shipped a pricing page promising a
// number nothing enforced.

// A single allowance, drawn. The bar is deliberately the same shape for traffic
// and backups: they are the same question asked about different units, and two
// different treatments would invite reading them as two different kinds of
// limit.
function Allowance({ label, usedGb, ceilingGb, note }: {
    label: string;
    usedGb: number;
    ceilingGb: number;
    note?: string;
}) {
    const pct = usagePct(usedGb, ceilingGb);
    const tone = pct >= 100 ? 'error' : pct >= 80 ? 'warning' : undefined;
    return (
        <div>
            <div className="flex items-baseline justify-between gap-3">
                <span className="text-[13px] text-(--base-07)">{label}</span>
                <span className="text-sm font-mono tabular-nums text-(--base-09)">
                    {usedGb} <span className="text-(--base-06)">/ {ceilingGb > 0 ? `${ceilingGb} GB` : 'none'}</span>
                </span>
            </div>
            <Meter value={pct} min={0} max={100} aria-label={label} className="mt-2">
                <MeterTrack className="srv-meter-track">
                    <MeterIndicator className="srv-meter-fill" data-tone={tone} />
                </MeterTrack>
            </Meter>
            {note && <p className="mt-1.5 text-xs text-(--base-06)">{note}</p>}
        </div>
    );
}

export default function StoreAccountCard({ summary, onChanged, onError }: {
    summary: StoreAccountSummary;
    onChanged: () => void;
    onError: (message: string) => void;
}) {
    const [busy, setBusy] = useState<'backup' | null>(null);

    // A quiet storefront is its own state. Rendering an account with nothing in
    // it would read as "you have no subscription", which is the opposite of what
    // happened and sends the reader to buy something they already own.
    if (summary.reachable === false) {
        return (
            <div className="srv-surface p-5 flex items-start gap-2.5">
                <CloudOff size={16} className="text-(--base-06) mt-0.5 shrink-0" />
                <p className="text-sm text-(--base-07) leading-relaxed">
                    {summary.message || 'The store could not be reached, so your subscription details are not shown.'}
                </p>
            </div>
        );
    }

    if (!summary.subscribed) {
        return (
            <div className="srv-surface p-5">
                <p className="text-sm text-(--base-07)">
                    No active subscription on this account. Servers you run on your own hardware are
                    unaffected; a subscription is what buys nodes, addresses and the allowances below.
                </p>
            </div>
        );
    }

    const apply = async (change: { backup?: boolean }) => {
        setBusy('backup');
        const res = await setStoreBillingConsent(change);
        setBusy(null);
        if (!res.success) {
            onError(res.message || 'The change could not be saved.');
            return;
        }
        onChanged();
    };

    const units = [
        summary.nodes ? `${summary.nodes} node${summary.nodes === 1 ? '' : 's'}` : '',
        summary.routeOnly ? `${summary.routeOnly} route-only` : '',
    ].filter(Boolean).join(' + ');

    return (
        <div className="space-y-6">
            <div className="srv-surface p-5 space-y-5">
                <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="text-sm font-medium text-(--base-09)">
                        Subscription
                        {units && <span className="text-(--base-06)"> - {units}</span>}
                    </div>
                    <span className={`badge ${summary.status === 'active' ? 'badge-success' : 'badge-warning'}`}>
                        {summary.status || 'unknown'}
                    </span>
                </div>

                {summary.traffic && (
                    <Allowance
                        label="Traffic this period"
                        usedGb={summary.traffic.usedGb ?? 0}
                        ceilingGb={summary.traffic.ceilingGb ?? 0}
                        note={summary.traffic.cutOff ? 'Your servers are currently stopped for traffic.' : undefined}
                    />
                )}

                {summary.backup && (
                    <Allowance
                        label="Backup storage"
                        usedGb={summary.backup.usedGb ?? 0}
                        ceilingGb={summary.backup.ceilingGb ?? 0}
                        // Storage on a bucket the tenant connected is neither
                        // billed nor capped. Folding it into the bar would put
                        // somebody over an allowance for space we do not provide,
                        // and leaving it out entirely would make 400 GB read as
                        // nothing on a screen headed "backup storage".
                        note={summary.backup.ownStorageGb
                            ? `Plus ${summary.backup.ownStorageGb} GB on your own storage, which is neither counted nor billed.`
                            : undefined}
                    />
                )}
            </div>

            <div className="srv-surface p-5 space-y-4">
                <div className="text-sm font-medium text-(--base-09)">When an allowance runs out</div>
                <ConsentRow
                    title="Bill me for extra backup storage"
                    kind="backup"
                    state={backupSwitch(summary)}
                    busy={busy === 'backup'}
                    onChange={next => apply({ backup: next })}
                />
                {/* The traffic consent is NOT here. It sits with the traffic
                    bars under My infrastructure, where the person watching an
                    allowance fill up is the person deciding - and one switch in
                    one place, because two views of the same money decision can
                    disagree the moment a write fails. */}
                <p className="text-xs text-(--base-06)">
                    Metered traffic is switched beside your traffic usage, under My infrastructure.
                </p>
            </div>
        </div>
    );
}
