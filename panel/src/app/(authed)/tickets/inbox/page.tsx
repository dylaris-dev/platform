"use client";

import React, { useEffect, useState } from 'react';
import { Inbox, ShieldAlert, TriangleAlert } from 'lucide-react';
import { listInboxTickets, listTicketCategories, Ticket, TicketStatus, TicketCategory } from '@/lib/api/tickets';
import { useAppData } from '@/lib/AppDataContext';
import { Skeleton } from '@/components/Skeleton';
import TicketsDisabledBanner from '@/components/tickets/TicketsDisabledBanner';
import { StatusBadge, PriorityBadge } from '@/components/tickets/TicketBadges';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import Segmented from '@/components/ui/Segmented';
import Select from '@/components/ui/Select';
import { RowList, Row } from '@/components/ui/RowList';

type Scope = 'all' | 'mine' | 'team' | 'unassigned';
const SCOPES: { id: Scope; label: string }[] = [
    { id: 'all',        label: 'All' },
    { id: 'mine',       label: 'Assigned to me' },
    { id: 'team',       label: 'My team' },
    { id: 'unassigned', label: 'Unassigned' },
];

type StatusId = 'open_active' | 'waiting_user' | 'resolved' | 'closed' | 'all';
const STATUS_FILTERS: { id: StatusId; label: string; statuses?: TicketStatus[] }[] = [
    { id: 'open_active',  label: 'Active',       statuses: ['open', 'in_progress'] },
    { id: 'waiting_user', label: 'Waiting user', statuses: ['waiting_user'] },
    { id: 'resolved',     label: 'Resolved',     statuses: ['resolved'] },
    { id: 'closed',       label: 'Closed',       statuses: ['closed'] },
    { id: 'all',          label: 'All' },
];

export default function InboxPage() {
    const { user, featureFlags } = useAppData();
    const [scope, setScope] = useState<Scope>('all');
    const [status, setStatus] = useState<StatusId>('open_active');
    const [categoryId, setCategoryId] = useState<number | ''>('');
    const [tickets, setTickets] = useState<Ticket[]>([]);
    const [categories, setCategories] = useState<TicketCategory[]>([]);
    const [loading, setLoading] = useState(true);
    const [failed, setFailed] = useState(false);

    const isSupport = user?.role === 'support' || user?.isAdmin;

    useEffect(() => {
        listTicketCategories().then(res => {
            if (res.success) setCategories(res.categories || []);
        });
    }, []);

    useEffect(() => {
        if (!isSupport || !featureFlags.tickets) {
            setLoading(false);
            return;
        }
        let cancelled = false;
        (async () => {
            setLoading(true);
            const statusFilter = STATUS_FILTERS.find(s => s.id === status);
            const res = await listInboxTickets({
                scope,
                status: statusFilter?.statuses,
                categoryId: categoryId === '' ? undefined : Number(categoryId),
            });
            if (cancelled) return;
            setFailed(!res.success);
            if (res.success) setTickets(res.tickets || []);
            setLoading(false);
        })();
        return () => { cancelled = true; };
    }, [scope, status, categoryId, isSupport, featureFlags.tickets]);

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    if (!isSupport) {
        return <EmptyState tone="error" icon={<ShieldAlert size={20} />} title="The inbox is reserved for support and admin roles." className="mx-auto mt-8 max-w-md" />;
    }

    return (
        <div className="space-y-6">
            <PageHeader title="Support inbox" description="All tickets you can see across the platform." />

            <div className="flex flex-wrap items-center gap-3">
                <Segmented options={SCOPES} value={scope} onChange={setScope} ariaLabel="Ticket scope" />
                <Segmented options={STATUS_FILTERS} value={status} onChange={setStatus} ariaLabel="Filter tickets by status" />
                <Select
                    value={categoryId === '' ? '' : String(categoryId)}
                    onChange={v => setCategoryId(v ? Number(v) : '')}
                    options={[{ value: '', label: 'All categories' }, ...categories.map(c => ({ value: String(c.id), label: c.name }))]}
                    ariaLabel="Category"
                    className="w-48 sm:ml-auto"
                />
            </div>

            {loading ? (
                <div className="srv-surface space-y-1 p-3">
                    {Array.from({ length: 6 }).map((_, i) => <Skeleton key={i} className="h-12 w-full rounded-lg" />)}
                </div>
            ) : failed ? (
                <EmptyState tone="error" icon={<TriangleAlert size={20} />} title="Could not load the inbox" description="Check your connection and change a filter, or reload the page." />
            ) : tickets.length === 0 ? (
                <EmptyState icon={<Inbox size={20} />} title="No tickets match this filter." />
            ) : (
                <div className="srv-surface">
                    <RowList label="Inbox">
                        {tickets.map(t => (
                            <Row
                                key={t.id}
                                href={`/tickets/${t.id}`}
                                title={t.title}
                                meta={<>
                                    <span className="srv-num">#{t.id}</span> · {t.categoryName} · {t.username}
                                    {t.assignedName && <> · assigned: <span className="text-(--accent-light)">{t.assignedName}</span></>}
                                    {t.assignedTeam && !t.assignedName && <> · team: {t.assignedTeam}</>}
                                </>}
                                trailing={<span className="flex items-center gap-2"><PriorityBadge priority={t.priority} /><StatusBadge status={t.status} /></span>}
                            />
                        ))}
                    </RowList>
                </div>
            )}
        </div>
    );
}
