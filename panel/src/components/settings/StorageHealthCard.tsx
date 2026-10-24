"use client";

import { useEffect, useState } from 'react';
import { CircleCheck, TriangleAlert } from 'lucide-react';
import { getStorageReachStatus, type StorageFault } from '@/lib/api/storageReach';
import { fleetHealthySummary, statusLabel, statusRemedy } from '@/lib/storageReach';
import { Badge } from '@/components/ui/Badge';
import { SkeletonCard } from '@/components/Skeleton';
import { systemEvents } from '@/lib/systemEvents';
import { fmtDateTime } from '@/lib/time';

/**
 * Fleet storage health. A Core that fails its boot or periodic self-check
 * gates its own storage routes and records a fault; this is where an admin
 * sees that without reading a container log.
 *
 * Three distinct, non-overlapping states: a loading skeleton, a healthy
 * summary line, or a list of faulted Cores. A failed load renders as its own
 * warning state and never falls through to the healthy line - an empty
 * `faults` array from a request that did not actually succeed would read as
 * "all clear" when the truth is "unknown", which is worse than showing
 * nothing.
 */
export default function StorageHealthCard() {
    const [loading, setLoading] = useState(true);
    const [faults, setFaults] = useState<StorageFault[]>([]);
    const [online, setOnline] = useState<string[]>([]);
    const [error, setError] = useState('');

    useEffect(() => {
        let cancelled = false;
        const load = async () => {
            const res = await getStorageReachStatus();
            if (cancelled) return;
            if (res.success) {
                setFaults(res.faults ?? []);
                setOnline(res.onlineCores ?? []);
                setError('');
            } else {
                setError(res.message || 'Could not read storage health.');
            }
            setLoading(false);
        };
        load();
        const unsub = systemEvents.on('storagereach.changed', () => { load(); });
        return () => { cancelled = true; unsub(); };
    }, []);

    if (loading) return <SkeletonCard />;

    if (error) {
        return (
            <div className="alert alert-warning text-xs" role="alert">
                {error}
            </div>
        );
    }

    return (
        <section className="flex flex-col gap-3">
            <h3 className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                Fleet storage health
            </h3>

            {faults.length === 0 ? (
                <div className="flex items-center gap-2 text-xs text-(--base-07)" role="status">
                    <CircleCheck size={14} className="text-(--success-light)" />
                    <span>{fleetHealthySummary(online.length)}</span>
                </div>
            ) : (
                <div role="alert">
                    <ul className="flex flex-col gap-3">
                        {faults.map(f => (
                            <li key={f.coreId} className="alert alert-error text-xs flex flex-col gap-1.5">
                                <div className="flex items-center gap-2">
                                    <TriangleAlert size={14} />
                                    <Badge variant="warning">{statusLabel(f.status)}</Badge>
                                    <span className="font-mono text-[11px] text-(--base-07)">
                                        {f.hostname ? `${f.coreId} (${f.hostname})` : f.coreId}
                                    </span>
                                </div>
                                <span className="text-(--base-06)">
                                    {statusRemedy(f.status)}
                                    {f.missingPeers?.length ? ` Cannot see: ${f.missingPeers.join(', ')}.` : ''}
                                    {f.deniedPeers?.length ? ` Cannot write into: ${f.deniedPeers.join(', ')}.` : ''}
                                </span>
                                {f.detail && (
                                    <span className="font-mono text-[11px] text-(--base-05)">{f.detail}</span>
                                )}
                                <span className="text-(--base-05)">
                                    Failing since {fmtDateTime(f.since * 1000)}
                                </span>
                            </li>
                        ))}
                    </ul>
                </div>
            )}
        </section>
    );
}
