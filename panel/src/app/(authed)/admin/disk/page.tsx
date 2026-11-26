"use client";

import React, { Suspense, useState, useEffect, useCallback } from 'react';
import { useSearchParams } from 'next/navigation';
import { AlertTriangle, RefreshCw, Server } from 'lucide-react';
import { getNodes, Node } from '@/lib/api';
import { DiskAnalysisPanel } from '@/components/admin/DiskAnalysisPanel';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import EmptyState from '@/components/ui/EmptyState';
import Callout from '@/components/server/Callout';

function AdminDiskInner() {
    const searchParams = useSearchParams();
    const focusNodeId = searchParams.get('node');

    const [nodes, setNodes] = useState<Node[]>([]);
    const [loading, setLoading] = useState(true);
    const [loadFailed, setLoadFailed] = useState(false);
    const [autoExpandNodeId, setAutoExpandNodeId] = useState<number | null>(
        focusNodeId ? Number(focusNodeId) : null,
    );
    const [refreshKey, setRefreshKey] = useState(0);

    const refresh = useCallback(() => setRefreshKey(k => k + 1), []);

    useEffect(() => {
        setLoading(true);
        getNodes().then(res => {
            setNodes(res.success ? (res.nodes ?? []) : []);
            setLoadFailed(!res.success);
            setLoading(false);
        }).catch(() => { setLoadFailed(true); setLoading(false); });
    }, [refreshKey]);

    return (
        <div className="flex flex-col gap-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
                <p className="max-w-2xl text-[13px] text-(--base-06)">
                    Compare UUID folders on each node's disk against database records. Click a node to run the analysis.
                </p>
                <button onClick={refresh} className="btn btn-secondary btn-sm shrink-0">
                    <RefreshCw size={14} />
                    Refresh
                </button>
            </div>
            <Callout tone="warning" icon={<AlertTriangle size={16} />}>
                Deleting orphaned folders or DB entries is permanent. Verify before deleting.
            </Callout>
            <div className="space-y-3 pb-4">
                {loading ? (
                    <div className="space-y-3" aria-busy="true">
                        <SkeletonCard height="h-16" />
                        <SkeletonCard height="h-16" />
                    </div>
                ) : loadFailed ? (
                    <EmptyState
                        tone="error"
                        icon={<AlertTriangle size={20} />}
                        title="Could not load nodes"
                        description="The node list did not load, so there is nothing to analyse."
                        action={<button type="button" onClick={refresh} className="btn btn-secondary btn-sm"><RefreshCw size={14} /> Retry</button>}
                    />
                ) : nodes.length === 0 ? (
                    <EmptyState icon={<Server size={20} />} title="No nodes registered" />
                ) : (
                    nodes.map(n => (
                        <DiskAnalysisPanel
                            key={n.id}
                            node={n}
                            onOrphanDeleted={refresh}
                            autoLoad={autoExpandNodeId === n.id}
                            onAutoLoadConsumed={() => setAutoExpandNodeId(null)}
                        />
                    ))
                )}
            </div>
        </div>
    );
}

export default function AdminDiskPage() {
    return (
        <Suspense fallback={
            <div className="p-4 space-y-3">
                <SkeletonHeader />
                <SkeletonCard height="h-40" />
            </div>
        }>
            <AdminDiskInner />
        </Suspense>
    );
}
