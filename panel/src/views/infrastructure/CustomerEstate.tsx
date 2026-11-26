"use client";

import type { CustomerCounts, CustomerSummary } from './InfraCards';

/**
 * What customers run: BYON nodes, their links, their warp overlay peers.
 *
 * Counted and NOT judged. Every other health surface in this panel turns amber
 * when a fraction is short of whole, and this one deliberately does not: these
 * machines belong to tenants, sit where nobody here can reach them, and are
 * switched off for ordinary reasons. "3 of 4 up" is information; "3 of 4 up" in
 * amber is a claim that something is wrong with this platform, and an operator
 * who cannot act on a warning learns to stop reading warnings - including the
 * next one, which might be ours.
 *
 * For the same reason these numbers do not reach Settings, Status at all. That
 * page answers "is MY platform healthy", and a customer's hardware is not part
 * of the answer.
 */

function Count({ label, counts }: { label: string; counts?: CustomerCounts }) {
    const total = counts?.total ?? 0;
    const online = counts?.online;
    return (
        <div className="flex flex-col gap-1">
            <span className="srv-label">{label}</span>
            <span className="srv-num text-2xl font-medium leading-none text-(--base-09)">
                {online === null || online === undefined
                    ? total
                    : `${online}/${total}`}
            </span>
            <span className="text-xs text-(--base-06)">
                {/* Absent and zero are different answers, and only one of them
                    is safe to render as a fraction. A warp leader that has not
                    been updated reports no liveness at all, and printing 0/12
                    would read as every customer being down. */}
                {online === null || online === undefined
                    ? 'registered'
                    : 'online'}
            </span>
        </div>
    );
}

export default function CustomerEstate({ customers }: { customers: CustomerSummary | null }) {
    if (!customers) return null;
    const nothing =
        (customers.nodes?.total ?? 0) === 0 &&
        (customers.links?.total ?? 0) === 0 &&
        (customers.warps?.total ?? 0) === 0;
    if (nothing) return null;

    return (
        <div className="srv-surface p-5 flex flex-col gap-4">
            <div className="flex flex-col gap-1">
                <h2 className="text-[15px] font-semibold text-(--base-09)">Customer estate</h2>
                <span className="text-[13px] text-(--base-06) max-w-2xl leading-relaxed">
                    Machines your customers run themselves. Tracked here and nowhere else:
                    one of these being offline is not an incident on this platform, so it
                    never raises a warning and never counts against the status page.
                </span>
            </div>
            <div className="grid grid-cols-3 gap-4 max-w-md">
                <Count label="BYON nodes" counts={customers.nodes} />
                <Count label="Links" counts={customers.links} />
                <Count label="Warp peers" counts={customers.warps} />
            </div>
        </div>
    );
}
