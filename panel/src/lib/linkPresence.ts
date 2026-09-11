import { nodeKind, type NodeKindFields } from './nodeKind';

/**
 * Whether a node gets the "no Link" warning.
 *
 * The Link is the only way a player reaches a server behind the gateway, and a
 * node that stops managing its own (NODE_MANAGES_LINK=false) relies on a Link
 * deployed beside it. When that deployment is missing, nothing fails anywhere:
 * the node reports healthy and the servers run, unreachable. The node's own
 * count of Link containers on its host is the one signal that sees it.
 *
 * `linkCount` undefined is UNKNOWN, never zero: an offline node has no
 * heartbeat to read, and Core omits the field rather than inventing a 0.
 *
 * A customer's own machine is counted, never warned about (see nodeKind), so a
 * BYON node is excluded even though the count means the same thing there.
 */
export function linkMissing(
    gatewayRouting: boolean,
    node: NodeKindFields & { linkCount?: number | null },
): boolean {
    return gatewayRouting && node.linkCount === 0 && nodeKind(node) !== 'byon';
}

export const LINK_MISSING_MESSAGE =
    'No Link is running on this node, so players cannot reach its servers through the gateway. ' +
    'Deploy the Link service on this host, or set NODE_MANAGES_LINK=true on the node so it runs its own.';
