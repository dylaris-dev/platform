"use client";

// Admin-only audit log for ticket deletions. Reachable from the link added
// to TicketSettingsTab. Paginated server-side via limit/offset, filterable by
// the actor (deletedBy UUID). Read-only — there's no edit/restore flow,
// deletions are by-design final.

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { ArrowLeft, Trash2, ShieldAlert, RefreshCw, Loader2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listTicketDeletions, TicketDeletion } from '@/lib/api/tickets';
import { SkeletonCard, SkeletonHeader } from '@/components/Skeleton';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import { fmtDateTime } from '@/lib/time';

const PAGE_SIZE = 50;

export default function TicketDeletionLogPage() {
    const { user } = useAppData();
    const [rows, setRows] = useState<TicketDeletion[]>([]);
    const [total, setTotal] = useState(0);
    const [offset, setOffset] = useState(0);
    const [deletedByFilter, setDeletedByFilter] = useState('');
    const [committedFilter, setCommittedFilter] = useState('');
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const [error, setError] = useState('');

    const isAdmin = user?.isAdmin ?? false;

    const load = async (reset = false) => {
        if (!isAdmin) return;
        if (reset) setLoading(true);
        else setRefreshing(true);
        const res = await listTicketDeletions({
            limit: PAGE_SIZE,
            offset: reset ? 0 : offset,
            deletedBy: committedFilter || undefined,
        });
        setLoading(false);
        setRefreshing(false);
        if (res.success) {
            setRows(res.deletions || []);
            setTotal(res.total ?? 0);
            if (reset) setOffset(0);
        } else {
            setError(res.message || 'Load failed.');
        }
    };

    useEffect(() => {
        load(true);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [isAdmin, committedFilter]);

    useEffect(() => {
        if (offset === 0) return;
        load();
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [offset]);

    if (!isAdmin) {
        return (
            <EmptyState tone="error" icon={<ShieldAlert size={20} />} title="Admins only" />
        );
    }

    return (
        <div className="space-y-6 max-w-5xl">
            <div>
                <Link href="/settings/tickets" className="mb-3 inline-flex items-center gap-1.5 text-xs text-(--base-06) hover:text-(--accent-light)">
                    <ArrowLeft size={12} /> Back to ticket settings
                </Link>
                <PageHeader
                    as="h2"
                    icon={<Trash2 size={18} />}
                    title="Ticket deletion log"
                    description="Append-only audit trail of admin-driven ticket deletions. Snapshot fields are preserved even if the originating user or category is later removed."
                    actions={
                        <button
                            type="button"
                            onClick={() => load(true)}
                            disabled={refreshing}
                            className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                        >
                            {refreshing ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />}
                            Refresh
                        </button>
                    }
                />
            </div>

            <form
                onSubmit={e => {
                    e.preventDefault();
                    setCommittedFilter(deletedByFilter.trim());
                }}
                className="flex flex-wrap items-end gap-3"
            >
                <div className="flex min-w-72 flex-col gap-1.5">
                    <label htmlFor="deletion-log-actor" className="srv-label">Deleted by (admin UUID)</label>
                    <input
                        id="deletion-log-actor"
                        type="text"
                        value={deletedByFilter}
                        onChange={e => setDeletedByFilter(e.target.value)}
                        placeholder="00000000-0000-0000-0000-000000000000"
                        className="input-mono w-full"
                    />
                </div>
                <button type="submit" className="btn btn-primary btn-sm">Filter</button>
                {committedFilter && (
                    <button
                        type="button"
                        onClick={() => { setDeletedByFilter(''); setCommittedFilter(''); }}
                        className="btn btn-secondary btn-sm"
                    >
                        Reset
                    </button>
                )}
            </form>

            {error && (
                <EmptyState tone="error" icon={<ShieldAlert size={20} />} title="The deletion log could not be loaded" description={error} />
            )}

            <div className="srv-surface overflow-hidden">
                {loading ? (
                    <div className="space-y-3 p-5">
                        <SkeletonHeader />
                        <SkeletonCard height="h-12" />
                        <SkeletonCard height="h-12" />
                        <SkeletonCard height="h-12" />
                    </div>
                ) : rows.length === 0 ? (
                    <EmptyState
                        bare
                        icon={<Trash2 size={20} />}
                        title={`No deletions recorded${committedFilter ? ' for this filter' : ''}`}
                    />
                ) : (
                    <RowList label="Deleted tickets">
                        {rows.map(r => (
                            <Row
                                key={r.id}
                                title={<><span className="font-mono text-(--base-06)">#{r.ticketId}</span> <span title={r.ticketSubject}>{r.ticketSubject}</span></>}
                                meta={<>Owner {r.ownerUsername} &middot; deleted by {r.deletedByName}{r.ipAddress ? <> &middot; <span className="font-mono">{r.ipAddress}</span></> : null}</>}
                                trailing={<span className="font-mono" title={r.deletedAt}>{formatTimestamp(r.deletedAt)}</span>}
                            />
                        ))}
                    </RowList>
                )}

                {!loading && total > PAGE_SIZE && (
                    <div className="flex items-center justify-between border-t border-(--base-03) px-4 py-3 text-xs text-(--base-06)">
                        <span>
                            Showing {offset + 1}&ndash;{Math.min(offset + rows.length, total)} of {total}
                        </span>
                        <div className="flex gap-2">
                            <button
                                type="button"
                                onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                                disabled={offset === 0}
                                className="btn btn-secondary btn-sm"
                            >
                                Previous
                            </button>
                            <button
                                type="button"
                                onClick={() => setOffset(offset + PAGE_SIZE)}
                                disabled={offset + rows.length >= total}
                                className="btn btn-secondary btn-sm"
                            >
                                Next
                            </button>
                        </div>
                    </div>
                )}
            </div>
        </div>
    );
}

function formatTimestamp(iso: string): string {
    try {
        const d = new Date(iso);
        return fmtDateTime(d);
    } catch {
        return iso;
    }
}
