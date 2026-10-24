"use client";

import React, { useEffect, useState } from 'react';
import { getMyBilling, BillingStatus, MyTrafficStatus } from '@/lib/api/billing';
import { getMyUsage, EntitlementState } from '@/lib/api/usage';
import { useAppData } from '@/lib/AppDataContext';
import { trafficBannerState } from '@/components/trafficBanner';
import { AlertTriangle, ArrowRight } from 'lucide-react';
import { fmtDateTime } from '@/lib/time';

// BillingBanner is the non-dismissible red bar shown to a tenant in past_due or
// suspended. Clicking it goes to the payment URL. Renders nothing when active,
// so it is a no-op for paying tenants. Re-polls every 5 minutes so a status
// change surfaces without a reload.
//
// Gated on featureFlags.store, which is true only when Core has both STORE_URL
// and STORE_SHARED_KEY. A build with no storefront has no billing lifecycle to
// report, so the poll was a request every five minutes per open tab for an
// answer that is structurally always "active" - and the one path that could
// have made it say otherwise would have shown a self-hoster a pay-now bar with
// nowhere to pay.
export default function BillingBanner() {
    const { featureFlags } = useAppData();
    const [status, setStatus] = useState<BillingStatus | null>(null);
    const [graceUntil, setGraceUntil] = useState<string | null>(null);
    const [paymentUrl, setPaymentUrl] = useState('');
    const [traffic, setTraffic] = useState<MyTrafficStatus | null>(null);
    const [overLimit, setOverLimit] = useState<EntitlementState | null>(null);

    useEffect(() => {
        if (!featureFlags.store) return;
        let cancelled = false;
        const load = async () => {
            const res = await getMyBilling();
            if (cancelled || !res.success) return;
            setStatus(res.status || null);
            setGraceUntil(res.graceUntil || null);
            setPaymentUrl(res.paymentUrl || '');
            setTraffic(res.traffic || null);
        };
        load();
        const t = setInterval(load, 5 * 60 * 1000);
        return () => { cancelled = true; clearInterval(t); };
    }, [featureFlags.store]);

    // Over-limit is polled separately from payment because it is a separate
    // problem: a tenant who downgraded is paying perfectly well and still holding
    // more than they bought. It is also NOT gated on the store - an admin grant
    // can be lowered on a self-host too, and the cutoff is just as real there.
    useEffect(() => {
        let cancelled = false;
        const load = async () => {
            const res = await getMyUsage();
            if (cancelled || !res.success) return;
            setOverLimit(res.entitlementState?.overLimit ? res.entitlementState : null);
        };
        load();
        const t = setInterval(load, 5 * 60 * 1000);
        return () => { cancelled = true; clearInterval(t); };
    }, []);

    // Over-limit outranks dunning: it names a nearer deadline with a harder
    // consequence, and stacking two red bars reads as one broken page.
    if (overLimit) {
        return (
            <div className="shrink-0">
                <div className="flex items-center justify-center gap-3 bg-(--error) text-white px-4 py-2.5 text-sm font-medium shadow-md">
                    <AlertTriangle size={18} className="shrink-0" />
                    <span>
                        You are using more than your subscription covers. Reduce what you have or
                        raise your subscription by{' '}
                        <strong>{fmtDateTime(overLimit.cutoffAt)}</strong>, or
                        everything on your account is disconnected. Nothing is deleted.
                    </span>
                </div>
            </div>
        );
    }

    if (!featureFlags.store) return null;

    // The traffic states are decided in trafficBanner.ts, which has a test. The
    // two distinctions it makes are both easy to get wrong from inside a render:
    // "over the ceiling" must be read off the raw gigabytes rather than the
    // truncated percentage, and "over" is not yet "stopped" because the store's
    // guard runs hourly.
    const trafficState = trafficBannerState(traffic, status);

    // Either traffic state outranks the dunning message. The store stops a tenant
    // with the same suspend action non-payment uses, so a traffic cutoff would
    // otherwise read as "pay up" to someone who has paid perfectly well. A tenant
    // who is both over the ceiling AND behind on payment is shown this one: it is
    // the blocker that paying alone will not clear.
    if (traffic && (trafficState === 'stopped' || trafficState === 'over')) {
        const stopped = trafficState === 'stopped';
        const inner = (
            <div className="flex items-center justify-center gap-3 bg-(--error) text-white px-4 py-2.5 text-sm font-medium shadow-md">
                <AlertTriangle size={18} className="shrink-0" />
                <span>
                    {stopped ? 'Your services are stopped: you have used ' : 'You have used '}
                    <strong>{traffic.usedGb} GB</strong> of the {traffic.ceilingGb} GB your
                    subscription covers, and metered billing is off
                    {stopped
                        ? ', so we stopped rather than charging you. Turn metered billing on to start again. Nothing is deleted.'
                        : '. Your servers and routing stop shortly unless you turn metered billing on.'}
                </span>
                {paymentUrl && (
                    <span className="inline-flex items-center gap-1 font-semibold underline underline-offset-2 whitespace-nowrap">
                        Turn it on <ArrowRight size={15} />
                    </span>
                )}
            </div>
        );
        return paymentUrl
            ? <a href={paymentUrl} target="_blank" rel="noreferrer" className="block shrink-0 hover:brightness-110 transition-[filter]">{inner}</a>
            : <div className="shrink-0">{inner}</div>;
    }

    if (!status || status === 'active') {
        // Approaching the ceiling, with time left to act. Amber rather than red:
        // nothing has happened yet, and a red bar that stays red for a week is a
        // bar people stop reading.
        if (traffic && trafficState === 'approaching') {
            const inner = (
                <div className="flex items-center justify-center gap-3 bg-(--warning) text-black px-4 py-2.5 text-sm font-medium shadow-md">
                    <AlertTriangle size={18} className="shrink-0" />
                    <span>
                        You have used <strong>{traffic.pct}%</strong> of the traffic your
                        subscription covers ({traffic.usedGb} of {traffic.ceilingGb} GB). Metered
                        billing is off, so once you pass it your servers and routing STOP rather
                        than being billed. Turn metered billing on to keep running.
                    </span>
                    {paymentUrl && (
                        <span className="inline-flex items-center gap-1 font-semibold underline underline-offset-2 whitespace-nowrap">
                            Turn it on <ArrowRight size={15} />
                        </span>
                    )}
                </div>
            );
            return paymentUrl
                ? <a href={paymentUrl} target="_blank" rel="noreferrer" className="block shrink-0 hover:brightness-110 transition-[filter]">{inner}</a>
                : <div className="shrink-0">{inner}</div>;
        }
        return null;
    }

    const suspended = status === 'suspended';
    const message = suspended
        ? 'Your services are suspended for non-payment. Your data and backups are safe — settle payment to reactivate.'
        : `Payment required. Your services will be suspended${graceUntil ? ` on ${fmtDateTime(graceUntil)}` : ''}. Pay now to avoid interruption.`;

    const inner = (
        <div className="flex items-center justify-center gap-3 bg-(--error) text-white px-4 py-2.5 text-sm font-medium shadow-md">
            <AlertTriangle size={18} className="shrink-0" />
            <span>{message}</span>
            {paymentUrl && (
                <span className="inline-flex items-center gap-1 font-semibold underline underline-offset-2 whitespace-nowrap">
                    Pay now <ArrowRight size={15} />
                </span>
            )}
        </div>
    );

    // Whole bar is the call-to-action. No dismiss control by design.
    if (paymentUrl) {
        return (
            <a href={paymentUrl} target="_blank" rel="noreferrer" className="block shrink-0 hover:brightness-110 transition-[filter]">
                {inner}
            </a>
        );
    }
    return <div className="shrink-0">{inner}</div>;
}
