import type { BillingStatus, MyTrafficStatus } from '@/lib/api/billing';

// TRAFFIC_WARN_PCT is where the amber warning starts. 80% leaves a fifth of the
// month's allowance to notice it and act, which for a server busy enough to get
// here is a day or two - not the hours a 95% threshold would give.
export const TRAFFIC_WARN_PCT = 80;

export type TrafficBannerState =
    | 'none'          // nothing to say
    | 'approaching'   // past the warn threshold, still free
    | 'over'          // past the ceiling, not stopped YET
    | 'stopped';      // past the ceiling and the store has stopped them

/**
 * Decides what the traffic banner should say.
 *
 * Two things here are easy to get wrong and both were, before this was pulled
 * out of the component and given a test:
 *
 *  1. "Over the ceiling" is compared on the RAW gigabytes, never on pct. pct is
 *     integer-truncated, so 1100 of 1100 GB reads as 100% while the store - which
 *     cuts at strictly MORE than the ceiling - has stopped nothing. Reading the
 *     percentage told a running tenant their servers were down.
 *  2. Being over is not the same as being stopped. The store's guard runs hourly,
 *     so there is a window where a tenant is past the ceiling and still running.
 *     Which one it is comes from the status Core actually holds, not from the
 *     number that will eventually cause it.
 *
 * A tenant who switched metered billing ON is never in trouble here, however
 * high the bar goes: they get billed, nothing stops.
 */
export function trafficBannerState(
    traffic: MyTrafficStatus | null | undefined,
    status: BillingStatus | null,
): TrafficBannerState {
    if (!traffic || traffic.billingEnabled) return 'none';
    if (traffic.ceilingGb <= 0) return 'none';
    if (traffic.usedGb > traffic.ceilingGb) {
        return status === 'suspended' ? 'stopped' : 'over';
    }
    return traffic.pct >= TRAFFIC_WARN_PCT ? 'approaching' : 'none';
}

export type DataBannerState = 'none' | 'warn' | 'throttled';

/**
 * Data traffic (beam + custom tabs) has its own line: past its allowance it is
 * slowed, never stopped, so it must not borrow the player banner's "stops".
 * Warned from 90 %, the same point Core notifies at.
 */
export function dataBannerState(traffic: MyTrafficStatus | null | undefined): DataBannerState {
    if (!traffic || traffic.dataBillingEnabled) return 'none';
    if (traffic.dataThrottled) return 'throttled';
    const data = traffic.pools?.find(p => p.kind === 'relay' && p.includedGb != null);
    return data && data.pct >= 90 ? 'warn' : 'none';
}
