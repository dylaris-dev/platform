// Pure helpers for Bedrock on routes (RoutesModal, BedrockRoutes).
import { setRouteEdition, type BedrockRouteEdition, type LinkRouteBedrock, type ServerBedrock } from './api';
import type { SegmentedOption } from '@/components/ui/Segmented';
import { refusalText } from './gatewayProvider';
import { fmtDate, fmtDateTime } from './time';

export type Edition = 'java' | 'bedrock' | 'both';

/** A route Core has no row for is Java only. */
export function editionOf(b: Pick<ServerBedrock, 'routes'> | null, domain: string): Edition {
    const r = b?.routes.find(x => x.domain === domain);
    if (!r) return 'java';
    if (r.java && r.bedrock) return 'both';
    return r.bedrock ? 'bedrock' : 'java';
}

/** A route-only address's stored flags as an edition; no flags is Java only. */
export function linkEdition(v: { java: boolean; bedrock: boolean } | undefined): Edition {
    if (!v || !v.bedrock) return 'java';
    return v.java ? 'both' : 'bedrock';
}

export function editionFlags(e: Edition): { java: boolean; bedrock: boolean } {
    return { java: e !== 'bedrock', bedrock: e !== 'java' };
}

/** Whether this caller may give a route Bedrock right now. */
export function canAddBedrock(b: Pick<ServerBedrock, 'allowed' | 'available'>): boolean {
    return b.allowed && b.available;
}

/** Whether the server summary lets this address be given Bedrock. The summary's
 *  refusal is about its `address` only: Core decides every other address when
 *  it is added (its own region, or the DNS check of an own domain), so those
 *  are left to the add request, whose refusal is shown. */
export function canAddBedrockTo(b: Pick<ServerBedrock, 'allowed' | 'available'> & { address?: string }, domain: string): boolean {
    return b.allowed && (b.available || (!!b.address && domain !== b.address));
}

/** Shown when Bedrock can be added to one of `domains` (the server's
 *  addresses), or when a route already has it: an owner must always be able
 *  to take it away again. */
export function showBedrock(b: Pick<ServerBedrock, 'allowed' | 'available' | 'enabled'> & { address?: string }, domains: string[] = []): boolean {
    return canAddBedrock(b) || b.enabled || domains.some(d => canAddBedrockTo(b, d));
}

/** The three choices for one route. Options that would ADD Bedrock are
 *  disabled while it cannot be added; the current one never is. `takenBy` is
 *  the managed gateway's one-address rule: the other address that has it. */
export function editionOptions(current: Edition, canAdd: boolean, why: string, takenBy?: string | null): SegmentedOption<Edition>[] {
    const adds = (e: Edition) => editionFlags(e).bedrock && !editionFlags(current).bedrock;
    if (takenBy) {
        canAdd = false;
        why = `This server already has a Bedrock address: ${takenBy}.`;
    }
    return ([
        { id: 'java', label: 'Java', hint: 'Java players only.' },
        { id: 'both', label: 'Java + Bedrock', hint: 'Java and Bedrock players on the same address.' },
        { id: 'bedrock', label: 'Bedrock', hint: 'Bedrock players only. Java players get a notice.' },
    ] as SegmentedOption<Edition>[]).map(o => (adds(o.id) && !canAdd ? { ...o, disabled: true, hint: why } : o));
}

/** The visible line under an address whose options that add Bedrock are
 *  disabled, so the reason is not only a hover hint on them: the managed
 *  gateway's one-address rule (`takenBy`), else `refusal`, the summary's
 *  reason when the caller found it disables this address. null otherwise. */
export function editionBlockedNote(current: Edition, takenBy?: string | null, refusal?: string | null): string | null {
    if (editionFlags(current).bedrock) return null;
    if (takenBy) {
        return `This server already has a Bedrock address: ${takenBy}. With the managed gateway a server has one; take Bedrock off ${takenBy} first to give it to this address.`;
    }
    return refusal || null;
}

/** The other address of this server that has Bedrock, or null. */
export function bedrockTakenElsewhere(b: Pick<ServerBedrock, 'routes'>, domain: string): string | null {
    return b.routes.find(r => r.bedrock && r.domain !== domain)?.domain ?? null;
}

/** The last stored DNS check of an own-domain address, as a visible line
 *  under it: Core's (or the gateway provider's) stored answer, never a lookup.
 *  null for an address without Bedrock, for one on the gateway's domains,
 *  and for a route-only address never checked (its view cannot tell the two
 *  apart). */
export function bedrockDNSNote(r: BedrockRouteEdition | LinkRouteBedrock): string | null {
    // Only an address WITH Bedrock is re-checked. Without it a stored row is a
    // refused add's miss, which says nothing true about this Java-only address.
    if (!r.bedrock) return null;
    const [status, checkedAt, failingSince, ownDomain] = 'has_key' in r
        ? [r.dns_status, r.dns_checked_at, r.dns_failing_since, false]
        : [r.dnsStatus, r.dnsCheckedAt, r.dnsFailingSince, !!r.ownDomain];
    if (!status) return ownDomain ? 'Not checked yet.' : null;
    if (status === 'ok') return `DNS checked ${checkedAt ? fmtDateTime(checkedAt) : 'recently'}: points at the gateway.`;
    const since = failingSince || checkedAt;
    return `No longer points at the gateway${since ? ` since ${fmtDate(since)}` : ''}. Bedrock players cannot reach this address until the record is back.`;
}

/** Why Bedrock cannot be added, for the disabled options' hint. */
export function addBlockedReason(b: Pick<ServerBedrock, 'allowed' | 'available' | 'refusal'>): string {
    if (!b.allowed) return 'Bedrock is turned off on this platform.';
    if (!b.available) return b.refusal || 'Bedrock is not available for this server.';
    return '';
}

/** A warning for a route whose Bedrock is paused by the platform switch. */
export function pausedNotice(e: Edition, active: boolean): string | null {
    if (active || e === 'java') return null;
    return e === 'bedrock'
        ? 'Bedrock is turned off on this platform, so nobody can join through this address. Switch it to Java to use it.'
        : 'Bedrock is turned off on this platform; this address takes Java players only until it is back.';
}

/** The Floodgate build that matches the server's loader. */
export function floodgateLink(loader: string): { label: string; href: string } {
    return loader === 'mods'
        ? { label: 'Floodgate-Modded', href: 'https://modrinth.com/mod/floodgate' }
        : { label: 'Floodgate for Spigot/Paper', href: 'https://geysermc.org/download?project=floodgate' };
}

/** Saves one route's edition. Returns the error to show, or null on success.
 *  fetchAPI resolves on 4xx/5xx with {success:false}, so the 409 sentence
 *  from Core arrives as message/error. */
export async function changeEdition(serverId: number, domain: string, e: Edition): Promise<string | null> {
    return (await saveEdition(serverId, domain, e)).error;
}

/** changeEdition plus what the managed gateway adds to a success: whether
 *  the server must restart to load a new Floodgate key, and a warning when
 *  the key could not be handed to the node yet. */
export async function saveEdition(serverId: number, domain: string, e: Edition):
    Promise<{ error: string | null; restartRequired: boolean; warning: string }> {
    try {
        const res = await setRouteEdition(serverId, domain, editionFlags(e));
        if (res?.success === false) {
            return { error: refusalText(res, 'The route could not be changed.'), restartRequired: false, warning: '' };
        }
        return { error: null, restartRequired: !!res?.restartRequired, warning: res?.warning || '' };
    } catch {
        return { error: 'The request could not be sent.', restartRequired: false, warning: '' };
    }
}
