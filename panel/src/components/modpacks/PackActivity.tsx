"use client";

import { useEffect, useState } from 'react';
import { History } from 'lucide-react';
import { getPackAudit, type PackAuditEvent } from '@/lib/api/packs';
import { packAuditText } from '@/lib/packAudit';
import { fmtDateTime } from '@/lib/time';
import { SkeletonText } from '@/components/Skeleton';

/**
 * Who changed what on a pack (packId) or on all of the owner's packs. Only the
 * owner (and an admin) can read it; for anyone else Core answers 403 and the
 * list stays empty.
 */
export default function PackActivity({ packId, showPack = false }: { packId?: number; showPack?: boolean }) {
    const [events, setEvents] = useState<PackAuditEvent[] | null>(null);
    useEffect(() => {
        let cancelled = false;
        getPackAudit(packId).then(e => { if (!cancelled) setEvents(e); });
        return () => { cancelled = true; };
    }, [packId]);

    return (
        <section className="card p-4">
            <h2 className="flex items-center gap-2 text-sm font-medium text-(--base-09) mb-3">
                <History size={14} className="text-(--accent-light)" /> Activity
            </h2>
            {events === null ? (
                <div className="space-y-2"><SkeletonText width="w-64" /><SkeletonText width="w-48" /><SkeletonText width="w-56" /></div>
            ) : events.length === 0 ? (
                <p className="text-xs text-(--base-06)">No changes recorded yet.</p>
            ) : (
                <ul className="space-y-1.5 max-h-72 overflow-y-auto">
                    {events.map(e => (
                        <li key={e.id} className="text-xs text-(--base-07) flex items-baseline gap-2">
                            <span className="text-(--base-05) font-mono shrink-0">{fmtDateTime(e.createdAt, { dateStyle: 'short', timeStyle: 'short' })}</span>
                            <span>
                                <span className="text-(--base-09) font-medium">{e.actorName || 'Someone'}</span>{' '}
                                {packAuditText(e.action, e.detail)}
                                {showPack && e.packName && <span className="text-(--base-06)"> on {e.packName}</span>}
                            </span>
                        </li>
                    ))}
                </ul>
            )}
        </section>
    );
}
