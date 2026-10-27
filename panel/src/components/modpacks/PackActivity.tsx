"use client";

import { useEffect, useState } from 'react';
import { History } from 'lucide-react';
import { getPackAudit, type PackAuditEvent } from '@/lib/api/packs';
import { packAuditText } from '@/lib/packAudit';
import { fmtDateTime } from '@/lib/time';
import { SkeletonText } from '@/components/Skeleton';
import { systemEvents } from '@/lib/systemEvents';

/**
 * Who changed what on a pack (packId) or on all of the owner's packs. Only the
 * owner (and an admin) can read it; for anyone else Core answers 403 and the
 * list stays empty.
 */
export default function PackActivity({ packId, showPack = false, compact = false, limit }: { packId?: number; showPack?: boolean; compact?: boolean; limit?: number }) {
    const [events, setEvents] = useState<PackAuditEvent[] | null>(null);
    useEffect(() => {
        let cancelled = false;
        const load = () => getPackAudit(packId).then(e => { if (!cancelled) setEvents(e); });
        load();
        const dispose = ['packs.changed', 'pack_builds.changed', 'pack_content.changed'].map(event => systemEvents.on(event, () => { load(); }));
        return () => { cancelled = true; dispose.forEach(fn => fn()); };
    }, [packId]);

    return (
        <section className={compact ? '' : 'card p-4'}>
            <h2 className="flex items-center gap-2 text-sm font-medium text-(--base-09) mb-3">
                <History size={14} className="text-(--accent-light)" /> Activity
            </h2>
            {events === null ? (
                <div className="space-y-2"><SkeletonText width="w-64" /><SkeletonText width="w-48" /><SkeletonText width="w-56" /></div>
            ) : events.length === 0 ? (
                <p className="text-xs text-(--base-06)">No changes recorded yet.</p>
            ) : (
                <ul className={compact ? 'space-y-3' : 'space-y-1.5 max-h-96 overflow-y-auto'}>
                    {events.slice(0, limit).map(e => (
                        <li key={e.id} className={`text-xs text-(--base-07) ${compact ? 'space-y-1' : 'flex items-baseline gap-2'}`}>
                            <span className={`text-(--base-05) font-mono shrink-0 ${compact ? "block text-[10px]" : ""}`}>{fmtDateTime(e.createdAt, { dateStyle: 'short', timeStyle: 'short' })}</span>
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
