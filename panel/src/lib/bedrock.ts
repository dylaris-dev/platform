// Pure helpers for Bedrock on routes (RoutesModal, BedrockRoutes).
import { setRouteEdition, type ServerBedrock } from './api';
import type { SegmentedOption } from '@/components/ui/Segmented';

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

/** Shown when Bedrock can be added here, or when a route already has it: an
 *  owner must always be able to take it away again. */
export function showBedrock(b: Pick<ServerBedrock, 'allowed' | 'available' | 'enabled'>): boolean {
    return canAddBedrock(b) || b.enabled;
}

/** The three choices for one route. Options that would ADD Bedrock are
 *  disabled while it cannot be added; the current one never is. */
export function editionOptions(current: Edition, canAdd: boolean, why: string): SegmentedOption<Edition>[] {
    const adds = (e: Edition) => editionFlags(e).bedrock && !editionFlags(current).bedrock;
    return ([
        { id: 'java', label: 'Java', hint: 'Java players only.' },
        { id: 'both', label: 'Java + Bedrock', hint: 'Java and Bedrock players on the same address.' },
        { id: 'bedrock', label: 'Bedrock', hint: 'Bedrock players only. Java players get a notice.' },
    ] as SegmentedOption<Edition>[]).map(o => (adds(o.id) && !canAdd ? { ...o, disabled: true, hint: why } : o));
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
    try {
        const res = await setRouteEdition(serverId, domain, editionFlags(e));
        if (res?.success === false) return res.message || res.error || 'The route could not be changed.';
        return null;
    } catch {
        return 'The request could not be sent.';
    }
}
