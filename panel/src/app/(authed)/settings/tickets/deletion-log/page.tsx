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
            <main className="flex-1 flex items-center justify-center p-6 text-(--base-06)">
                <div className="text-center max-w-md">
                    <ShieldAlert size={28} className="mx-auto text-(--error-light)" />
                    <p className="mt-3 text-sm">Admins only.</p>
                </div>
            </main>
        );
    }

    return (
        <div className="space-y-6 max-w-5xl">
            <header className="flex items-start justify-between gap-4 flex-wrap">
                <div>
                    <Link href="/settings/tickets" className="inline-flex items-center gap-1.5 text-xs text-(--base-06) hover:text-(--accent-light) mb-3">
                        <ArrowLeft size={12} /> Back to ticket settings
                    </Link>
                    <h2 className="h-section flex items-center gap-2">
                        <Trash2 size={18} className="text-(--error-light)" /> Ticket Deletion Log
                    </h2>
                    <p className="text-sm text-(--base-06) mt-1">
                        Append-only audit trail of admin-driven ticket deletions. Snapshot fields are preserved even if the originating user or category is later removed.
                    </p>
                </div>
                <button
                    type="button"
                    onClick={() => load(true)}
                    disabled={refreshing}
                    className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                >
                    {refreshing ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />}
                    Refresh
                </button>
            </header>

            {/* Filter bar */}
            <div className="card p-4 border border-(--base-03)">
                <form
                    onSubmit={e => {
                        e.preventDefault();
                        setCommittedFilter(deletedByFilter.trim());
                    }}
                    className="flex items-end gap-3 flex-wrap"
                >
                    <div className="flex flex-col gap-[5px] min-w-72">
                        <label className="mono-label">Deleted by (admin UUID)</label>
                        <input
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
            </div>

            {error && (
                <p className="text-sm text-(--error-light)">{error}</p>
            )}

            {/* Table */}
            <div className="card border border-(--base-03) overflow-hidden">
                {loading ? (
                    <div className="p-5 space-y-3">
                        <SkeletonHeader />
                        <SkeletonCard height="h-12" />
                        <SkeletonCard height="h-12" />
                        <SkeletonCard height="h-12" />
                    </div>
                ) : rows.length === 0 ? (
                    <div className="p-10 text-center text-sm text-(--base-06)">
                        No deletions recorded{committedFilter ? ' (for this filter)' : ''}.
                    </div>
                ) : (
                    <div className="overflow-x-auto">
                        <table className="w-full text-sm">
                            <thead className="bg-(--base-02) text-(--base-06) text-[10px] uppercase tracking-[0.08em]">
                                <tr>
                                    <th className="text-left px-4 py-2 font-mono">Datum</th>
                                    <th className="text-left px-4 py-2 font-mono">Ticket</th>
                                    <th className="text-left px-4 py-2 font-mono">Subject</th>
                                    <th className="text-left px-4 py-2 font-mono">Owner</th>
                                    <th className="text-left px-4 py-2 font-mono">Deleted by</th>
                                    <th className="text-left px-4 py-2 font-mono">IP</th>
                                </tr>
                            </thead>
                            <tbody>
                                {rows.map(r => (
                                    <tr key={r.id} className="border-t border-(--base-03) hover:bg-(--base-03)/30">
                                        <td className="px-4 py-2 text-(--base-07) whitespace-nowrap" title={r.deletedAt}>
                                            {formatTimestamp(r.deletedAt)}
                                        </td>
                                        <td className="px-4 py-2 font-mono text-(--base-06)">#{r.ticketId}</td>
                                        <td className="px-4 py-2 text-(--base-09) max-w-md truncate" title={r.ticketSubject}>
                                            {r.ticketSubject}
                                        </td>
                                        <td className="px-4 py-2 font-mono text-(--base-08)">{r.ownerUsername}</td>
                                        <td className="px-4 py-2 font-mono text-(--base-08)">{r.deletedByName}</td>
                                        <td className="px-4 py-2 font-mono text-(--base-06) text-xs">{r.ipAddress || '—'}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                )}

                {/* Pagination footer */}
                {!loading && total > PAGE_SIZE && (
                    <div className="border-t border-(--base-03) px-4 py-3 flex items-center justify-between text-xs text-(--base-06)">
                        <span>
                            Showing {offset + 1}–{Math.min(offset + rows.length, total)} of {total}
                        </span>
                        <div className="flex gap-2">
                            <button
                                type="button"
                                onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
                                disabled={offset === 0}
                                className="btn btn-secondary btn-sm disabled:opacity-40"
                            >
                                Previous
                            </button>
                            <button
                                type="button"
                                onClick={() => setOffset(offset + PAGE_SIZE)}
                                disabled={offset + rows.length >= total}
                                className="btn btn-secondary btn-sm disabled:opacity-40"
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
