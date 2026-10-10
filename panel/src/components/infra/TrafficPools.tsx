"use client";

import { useCallback, useEffect, useState } from 'react';
import { Gauge, AlertTriangle, Loader2 } from 'lucide-react';
import { getMyBilling, type MyTrafficStatus, type TrafficPool } from '@/lib/api/billing';
import { getStoreAccountSummary, setStoreBillingConsent, type StoreAccountSummary } from '@/lib/api/store';
import { trafficSwitch } from '@/lib/storeAccount';
import ConsentRow from '@/components/billing/ConsentRow';

/**
 * What the tenant has used against what they hold.
 *
 * Two boxes, because the platform meters two different things and they are
 * neither priced nor capped together: player traffic at the edge, and the file
 * transfers beam carries. Inside each, one bar per allowance - player traffic is
 * capped per REGION, so a tenant can sit at 30% of three of them and 130% of a
 * fourth, and it is the fourth that stops them. A single summed bar is the one
 * number that cannot say which.
 *
 * The bars are drawn against the INCLUDED allowance, not the fair-use ceiling.
 * The buffer is grace rather than allowance: a warning that only appears past it
 * arrives after the point where the tenant could still have done something.
 *
 * The consent switch lives here rather than only on the store page: the person
 * watching an allowance fill up is the person deciding whether running out
 * costs money or stops the service.
 */

/** Bytes, formatted so a share under a gigabyte does not read as nothing. The
 *  wire carries the product split in BYTES for exactly this reason. */
export function formatBytes(n: number): string {
    if (!Number.isFinite(n) || n <= 0) return '0 MB';
    if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(n < 10_000_000_000 ? 1 : 0)} GB`;
    if (n >= 1_000_000) return `${Math.round(n / 1_000_000)} MB`;
    return `${Math.max(1, Math.round(n / 1000))} KB`;
}

/** The two products a tenant can hold, named as the infrastructure tabs name
 *  them so the split maps onto the screens it came from. */
export const PRODUCT_LABEL: Record<string, string> = {
    byon: 'Bring your own node',
    route: 'Protected addresses',
};

/** Split a pool by product, largest first. The empty product is rows written
 *  before the split existed: real bytes with no attribution, shown as such
 *  rather than folded into one of the two, which would be a wrong answer where
 *  this is an honest missing one. */
export function productShares(p: TrafficPool): { product: string; label: string; bytes: number }[] {
    return Object.entries(p.byProductBytes || {})
        .filter(([, bytes]) => bytes > 0)
        .map(([product, bytes]) => ({
            product,
            label: PRODUCT_LABEL[product] || 'Not attributed',
            bytes,
        }))
        .sort((a, b) => b.bytes - a.bytes);
}

/** The notice a pool has earned. Kept beside the bar so the two never disagree. */
export function poolTone(warn: number): { bar: string; text: string; label: string } | null {
    if (warn >= 100) return { bar: 'bg-(--error)', text: 'text-(--error-light)', label: 'over the allowance' };
    if (warn >= 90) return { bar: 'bg-(--error)', text: 'text-(--error-light)', label: '90% used' };
    if (warn >= 80) return { bar: 'bg-(--warning)', text: 'text-(--warning)', label: '80% used' };
    return null;
}

/** Whether a pool has a ceiling to be a percentage of. A pool without one is
 *  still SHOWN - it moved real bytes, and hiding it is how "where is my data
 *  traffic" happens - it just gets a number instead of a bar. */
export function isCapped(p: TrafficPool): boolean {
    return p.includedGb !== null && p.includedGb > 0;
}

/** The region line under a pool. "*" is the region a non-regional pool is
 *  stored under; showing it raw would put a literal asterisk in front of the
 *  tenant as if it meant something. */
export function poolRegion(p: TrafficPool): string {
    return p.region === '*' ? 'All regions' : p.region;
}

/** The boxes, in the order they are drawn. A kind with no pools at all is
 *  dropped: an empty box headed "Data traffic" says less than no box. */
export function boxesFor(pools: TrafficPool[] | undefined): { kind: string; title: string; blurb: string; pools: TrafficPool[] }[] {
    const all = pools || [];
    return [
        {
            kind: 'edge',
            title: 'Player traffic',
            blurb: 'What players move to and from your servers, counted at the edge that served them. Capped per region.',
        },
        {
            kind: 'relay',
            title: 'Data traffic',
            blurb: 'File transfers carried by beam - uploads, downloads and backups. One pool for all regions, and only servers on your own machines produce it.',
        },
    ]
        .map(b => ({ ...b, pools: all.filter(p => p.kind === b.kind) }))
        .filter(b => b.pools.length > 0);
}

function PoolBar({ p }: { p: TrafficPool }) {
    const capped = isCapped(p);
    const tone = capped ? poolTone(p.warn) : null;
    // The BAR is clamped so it cannot overflow its track; the NUMBER beside it
    // is not, because someone at 300% has to be told 300% rather than shown a
    // reassuring full bar.
    const width = Math.min(100, Math.max(0, p.pct));
    const shares = productShares(p);

    return (
        <li>
            <div className="flex flex-wrap items-baseline justify-between gap-2 mb-1.5">
                <span className="text-xs text-(--base-08)">{poolRegion(p)}</span>
                <span className="text-xs text-(--base-06) tabular-nums">
                    <span className={tone ? tone.text : 'text-(--base-08)'}>{p.usedGb} GB</span>
                    {capped
                        ? <>{' of '}{p.includedGb} GB</>
                        : <span className="ml-1.5">used, no allowance set</span>}
                    {tone && <span className={`ml-2 ${tone.text}`}>{tone.label}</span>}
                </span>
            </div>
            {capped && (
                <div
                    className="h-1.5 rounded-full bg-(--base-03) overflow-hidden"
                    role="progressbar"
                    aria-valuenow={p.pct}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-label={`${poolRegion(p)}: ${p.usedGb} of ${p.includedGb} GB`}
                >
                    <div
                        className={`h-full rounded-full transition-all ${tone ? tone.bar : 'bg-(--accent)'}`}
                        style={{ width: `${width}%` }}
                    />
                </div>
            )}
            {shares.length > 0 && (
                <div className="flex flex-wrap gap-x-4 gap-y-1 mt-1.5">
                    {shares.map(s => (
                        <span key={s.product} className="text-xs text-(--base-06)">
                            {s.label}
                            <span className="ml-1.5 tabular-nums text-(--base-07)">{formatBytes(s.bytes)}</span>
                        </span>
                    ))}
                </div>
            )}
        </li>
    );
}

export default function TrafficPools() {
    const [traffic, setTraffic] = useState<MyTrafficStatus | null>(null);
    const [summary, setSummary] = useState<StoreAccountSummary | null>(null);
    const [loaded, setLoaded] = useState(false);
    const [busy, setBusy] = useState(false);
    const [consentError, setConsentError] = useState('');

    const load = useCallback(async () => {
        const [billing, store] = await Promise.all([
            // A failed read shows nothing rather than a zeroed bar: "we could not
            // ask" and "you have used nothing" lead to opposite reactions.
            getMyBilling().catch(() => null),
            getStoreAccountSummary().catch(() => null),
        ]);
        setTraffic(billing?.traffic || null);
        setSummary(store && store.success !== false ? store : null);
    }, []);

    useEffect(() => { load().finally(() => setLoaded(true)); }, [load]);

    const boxes = boxesFor(traffic?.pools);
    // Nothing metered against a limit - a self-hosted install, or a platform
    // where nobody has set an allowance yet. Silence is the honest answer.
    if (!loaded || !traffic || boxes.length === 0) return null;

    const metered = traffic.billingEnabled;

    const applyConsent = async (next: boolean) => {
        setBusy(true);
        setConsentError('');
        const res = await setStoreBillingConsent({ traffic: next });
        setBusy(false);
        if (!res.success) {
            setConsentError(res.message || 'The change could not be saved.');
            return;
        }
        load();
    };

    return (
        <div className="space-y-4">
            <div className="grid gap-4 lg:grid-cols-2">
                {boxes.map(box => (
                    <section key={box.kind} className="card p-5">
                        <div className="flex flex-wrap items-start justify-between gap-3 mb-4">
                            <div className="flex items-start gap-2.5 min-w-0">
                                <Gauge size={16} className="text-(--base-06) mt-0.5 shrink-0" />
                                <div className="min-w-0">
                                    <h2 className="text-sm font-medium text-(--base-09)">{box.title} this month</h2>
                                    <p className="text-xs text-(--base-06) mt-0.5">{box.blurb}</p>
                                </div>
                            </div>
                        </div>
                        <ul className="space-y-3">
                            {box.pools.map(p => <PoolBar key={`${p.kind}|${p.region}`} p={p} />)}
                        </ul>
                    </section>
                ))}
            </div>

            <div className="card p-5 space-y-3">
                {traffic.warn !== undefined && traffic.warn >= 80 && (
                    <span
                        className={`flex items-center gap-1.5 text-xs ${
                            traffic.warn >= 90 ? 'text-(--error-light)' : 'text-(--warning)'
                        }`}
                    >
                        <AlertTriangle size={13} />
                        {metered
                            ? 'Past this you are billed per additional TB.'
                            : 'Past this your servers are stopped, not billed.'}
                    </span>
                )}
                {summary
                    ? <ConsentRow
                        title="Bill me for extra traffic"
                        kind="traffic"
                        state={trafficSwitch(summary)}
                        busy={busy}
                        onChange={applyConsent}
                    />
                    : <p className="flex items-center gap-2 text-xs text-(--base-06)">
                        <Loader2 size={13} className="animate-spin" />
                        Reading your subscription to see whether metered traffic can be switched here.
                    </p>}
                {consentError && <p className="text-xs text-(--error-light)">{consentError}</p>}
            </div>
        </div>
    );
}
