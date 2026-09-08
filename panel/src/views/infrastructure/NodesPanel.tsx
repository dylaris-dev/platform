"use client";

import { useRouter } from 'next/navigation';
import { isKind, NODE_KIND_DESCRIPTION, type NodeKind } from '@/lib/nodeKind';
import { NodeCard } from './InfraCards';
import CustomerEstate from './CustomerEstate';
import { useInfra } from './context';

/**
 * One tab's worth of machines.
 *
 * The three tabs are the same component because they are the same list with a
 * different owner - the difference that matters is WHOSE hardware it is, and
 * that belongs in lib/nodeKind next to Core's own predicates, not in three
 * near-identical screens.
 *
 * This page READS. Deleting a node used to live here, which meant the one
 * irreversible node action sat among the graphs while every other node control
 * - configure, placement, reset pairing, setup values - lived in Settings ->
 * Nodes. Two screens to manage one machine, and the destructive half on the one
 * you open to check how things are going. Delete now lives with the rest.
 */

const EMPTY: Record<NodeKind, string> = {
    platform: 'No nodes registered',
    external: 'No machines of your own outside the swarm',
    byon: 'No customer has brought a machine yet',
};

export default function NodesPanel({ kind }: { kind: NodeKind }) {
    const infra = useInfra();
    const router = useRouter();

    const nodes = isKind(infra.nodes, kind);

    return (
        <>
            <p className="text-xs text-(--base-06) max-w-3xl -mt-1">{NODE_KIND_DESCRIPTION[kind]}</p>

            {/* Only on the BYON tab: it is the estate this summarises, and on the
                other two it would be a count of somebody else's machines under a
                heading about your own. */}
            {kind === 'byon' && <CustomerEstate customers={infra.customers} />}

            {nodes.length === 0 ? (
                <div className="card p-8 text-center text-(--base-06) text-sm">{EMPTY[kind]}</div>
            ) : (
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {nodes.map(node => (
                        <NodeCard
                            key={node.id}
                            node={node}
                            gatewayEnabled={infra.gatewayEnabled}
                            onNavigateToAdminDisk={(nodeId: number) => router.push(`/admin/disk?node=${nodeId}`)}
                        />
                    ))}
                </div>
            )}
        </>
    );
}
