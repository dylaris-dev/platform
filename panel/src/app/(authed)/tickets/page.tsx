"use client";

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { LifeBuoy, Plus, TriangleAlert } from 'lucide-react';
import { listMyTickets, Ticket, TicketStatus } from '@/lib/api/tickets';
import { Skeleton } from '@/components/Skeleton';
import { useAppData } from '@/lib/AppDataContext';
import TicketsDisabledBanner from '@/components/tickets/TicketsDisabledBanner';
import { StatusBadge, PriorityBadge, timeAgo } from '@/components/tickets/TicketBadges';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import Segmented from '@/components/ui/Segmented';
import { RowList, Row } from '@/components/ui/RowList';

type Filter = TicketStatus | 'all' | 'open_active';
const STATUS_FILTERS: { id: Filter; label: string }[] = [
    { id: 'open_active', label: 'Open' },
    { id: 'waiting_user', label: 'Waiting on you' },
    { id: 'resolved', label: 'Resolved' },
    { id: 'closed', label: 'Closed' },
    { id: 'all', label: 'All' },
];

export default function MyTicketsPage() {
    const { featureFlags } = useAppData();
    const [tickets, setTickets] = useState<Ticket[]>([]);
    const [loading, setLoading] = useState(true);
    const [failed, setFailed] = useState(false);
    const [filter, setFilter] = useState<Filter>('open_active');

    useEffect(() => {
        if (!featureFlags.tickets) return;
        let cancelled = false;
        const load = async () => {
            setLoading(true);
            const query: { status?: TicketStatus[] } = {};
            if (filter === 'open_active') query.status = ['open', 'in_progress'];
            else if (filter === 'waiting_user') query.status = ['waiting_user'];
            else if (filter === 'resolved') query.status = ['resolved'];
            else if (filter === 'closed') query.status = ['closed'];
            const res = await listMyTickets(query);
            if (cancelled) return;
            setFailed(!res.success);
            if (res.success) setTickets(res.tickets || []);
            setLoading(false);
        };
        load();
        return () => { cancelled = true; };
    }, [filter, featureFlags.tickets]);

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    const newTicket = (label: string) => (
        <Link href="/tickets/new" className="btn btn-primary"><Plus size={14} /> {label}</Link>
    );

    return (
        <div className="space-y-6">
            <PageHeader
                title="Tickets"
                description="Your open conversations with support."
                actions={newTicket('New ticket')}
            />

            <Segmented options={STATUS_FILTERS} value={filter} onChange={setFilter} ariaLabel="Filter tickets by status" />

            {loading ? (
                <div className="srv-surface space-y-1 p-3">
                    {Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-14 w-full rounded-lg" />)}
                </div>
            ) : failed ? (
                <EmptyState tone="error" icon={<TriangleAlert size={20} />} title="Could not load your tickets" description="Check your connection and try another filter, or reload the page." />
            ) : tickets.length === 0 ? (
                <EmptyState
                    icon={<LifeBuoy size={20} />}
                    title="No tickets here"
                    description={filter === 'open_active' ? 'You have no open tickets. Need help with something?' : 'No tickets match this filter yet.'}
                    action={newTicket('Create a ticket')}
                />
            ) : (
                <div className="srv-surface">
                    <RowList label="Your tickets">
                        {tickets.map(t => (
                            <Row
                                key={t.id}
                                href={`/tickets/${t.id}`}
                                title={t.title}
                                meta={<>
                                    <span className="srv-num">#{t.id}</span> · {t.categoryName || 'No category'}
                                    {t.serverName ? ` · ${t.serverName}` : (t.subjectRef ? ` · ${t.subjectRef}` : '')}
                                    {' · '}Updated {timeAgo(t.updatedAt)}
                                    {t.messageCount != null && t.messageCount > 0 && <> · {t.messageCount} message{t.messageCount === 1 ? '' : 's'}</>}
                                </>}
                                trailing={<span className="flex items-center gap-2"><StatusBadge status={t.status} /><PriorityBadge priority={t.priority} /></span>}
                            />
                        ))}
                    </RowList>
                </div>
            )}
        </div>
    );
}
