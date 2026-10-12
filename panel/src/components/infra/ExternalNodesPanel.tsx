"use client";

import { useState } from 'react';
import Link from 'next/link';
import { HardDrive, ArrowRight, Plus } from 'lucide-react';
import AddNodeModal from '@/components/AddNodeModal';

// ---------------------------------------------------------------------------
// The OPERATOR's own machines outside the swarm live under Settings -> Nodes ->
// External nodes: the list, adding a machine, its deploy file and a new key.
// This tab used to repeat the list without any of that, so it now only points
// there - one place instead of three. "How to add a node" stays, because it is
// also where joining a machine to the fleet is explained.
// ---------------------------------------------------------------------------

export default function ExternalNodesPanel() {
    const [showAdd, setShowAdd] = useState(false);
    return (
        <section className="card p-5 space-y-3">
            <h2 className="text-sm font-display font-semibold text-(--accent-light) flex items-center gap-2">
                <HardDrive size={15} /> External nodes
            </h2>
            <p className="text-sm text-(--base-07) max-w-2xl">
                Machines you run yourself that reach the platform over the warp overlay rather than sitting in the
                cluster. They are listed, added, deployed and given new keys in one place.
            </p>
            <div className="flex flex-wrap gap-2">
                <Link href="/settings/nodes?tab=external" className="btn btn-primary btn-sm">
                    Open External nodes <ArrowRight size={13} />
                </Link>
                <button type="button" onClick={() => setShowAdd(true)} className="btn btn-secondary btn-sm">
                    <Plus size={13} /> How to add a node
                </button>
            </div>
            {showAdd && <AddNodeModal onClose={() => setShowAdd(false)} />}
        </section>
    );
}
