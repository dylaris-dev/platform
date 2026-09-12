import { type NodeKindFields } from './nodeKind';

/**
 * Whether a node gets the "no Link" warning.
 *
 * The Link is the only way a player reaches a server behind the gateway, and a
 * node never runs one itself: it is deployed beside the node, as its own
 * service. When that deployment is missing, nothing fails anywhere - the node
 * reports healthy and the servers run, unreachable. The node's own count of Link
 * containers on its host is the one signal that sees it.
 *
 * `linkCount` undefined is UNKNOWN, never zero: an offline node has no
 * heartbeat to read, and Core omits the field rather than inventing a 0.
 *
 * A customer's own machine USED to be excluded here, because it always ran a
 * Link inside its node and a zero could hardly happen. The node starts none any
 * more, so zero on a BYON machine is no longer unlikely - it is what a customer
 * gets by updating their node image without redeploying their file, which is the
 * single likeliest way this breaks. Silence there would hide the common case.
 */
export function linkMissing(
    gatewayRouting: boolean,
    node: NodeKindFields & { linkCount?: number | null },
): boolean {
    return gatewayRouting && node.linkCount === 0;
}

export const LINK_MISSING_MESSAGE =
    'No Link is running on this node, so players cannot reach its servers through the gateway. ' +
    'Deploy the Link service on this host; the deploy file in the panel contains it.';
