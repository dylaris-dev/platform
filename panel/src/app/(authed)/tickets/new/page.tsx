"use client";

import React, { useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import Link from 'next/link';
import { ArrowLeft, Loader2, CircleAlert } from 'lucide-react';
import { listTicketCategories, createTicket, TicketCategory, TicketPriority } from '@/lib/api/tickets';
import { useAppData } from '@/lib/AppDataContext';
import TicketsDisabledBanner from '@/components/tickets/TicketsDisabledBanner';
import PageHeader from '@/components/ui/PageHeader';
import Select, { type SelectOption } from '@/components/ui/Select';
import { getNodes, getLinkRoutes } from '@/lib/api';
import '@/styles/tickets.css';

const PRIORITY_OPTIONS: SelectOption[] = [
    { value: 'low', label: 'Low' },
    { value: 'normal', label: 'Normal' },
    { value: 'high', label: 'High' },
    { value: 'urgent', label: 'Urgent' },
];

export default function NewTicketPage() {
    const router = useRouter();
    const { servers, featureFlags, gatewayEnabled, gatewayVisibility } = useAppData();
    const [categories, setCategories] = useState<TicketCategory[]>([]);
    const [loading, setLoading] = useState(true);
    const [categoryId, setCategoryId] = useState<number | ''>('');
    const [serverUuid, setServerUuid] = useState('');
    // One field for "what is this about". server_uuid only ever covered
    // servers, so someone whose own machine or protected address was the
    // problem had nowhere to say so and support had to infer it from the prose.
    const [subject, setSubject] = useState('');
    const [ownNodes, setOwnNodes] = useState<{ id: string; label: string }[]>([]);
    const [ownRoutes, setOwnRoutes] = useState<string[]>([]);
    const [title, setTitle] = useState('');
    const [firstMessage, setFirstMessage] = useState('');
    const [priority, setPriority] = useState<TicketPriority | ''>('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');

    useEffect(() => {
        if (!featureFlags.tickets) return;
        listTicketCategories().then(res => {
            if (res.success) setCategories(res.categories || []);
            setLoading(false);
        });
    }, [featureFlags.tickets]);

    // Best-effort: a reader who has neither simply gets fewer options. Failing
    // the form over an unreachable side list would block someone from asking
    // for help with the very thing that is broken.
    useEffect(() => {
        if (!featureFlags.tickets) return;
        let cancelled = false;
        getNodes().then(res => {
            if (cancelled || !res.success || !Array.isArray(res.nodes)) return;
            setOwnNodes((res.nodes as { name: string; displayName?: string }[])
                .map(n => ({ id: n.name, label: n.displayName || n.name })));
        }).catch(() => {});
        // Route-only addresses exist only on this platform's own gateway.
        if (gatewayEnabled && gatewayVisibility.tenancy) {
            getLinkRoutes().then(routes => {
                if (!cancelled && Array.isArray(routes)) setOwnRoutes(routes.map(r => r.domain));
            }).catch(() => {});
        }
        return () => { cancelled = true; };
    }, [featureFlags.tickets, gatewayEnabled, gatewayVisibility.tenancy]);

    const selectedCategory = categories.find(c => c.id === categoryId);
    const requiresServer = !!selectedCategory?.requiresServer;

    // When the category changes, snap the priority back to its default so
    // the user can opt to override only when they actually mean to.
    useEffect(() => {
        if (selectedCategory) {
            setPriority(selectedCategory.defaultPriority);
        }
    }, [selectedCategory]);

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        setError('');
        if (!categoryId) {
            setError('Pick a category to continue.');
            return;
        }
        if (requiresServer && !serverUuid) {
            setError('This category requires you to attach a server.');
            return;
        }
        const trimmedTitle = title.trim();
        const trimmedBody = firstMessage.trim();
        if (trimmedTitle.length < 3) {
            setError('Title must be at least 3 characters.');
            return;
        }
        if (trimmedBody.length < 1) {
            setError('Please describe your issue.');
            return;
        }
        setSubmitting(true);
        // One select, three shapes on the wire. A server keeps using serverUuid
        // (the category gate and every existing filter read that), so the
        // subject only has to name the machine or address cases.
        const [kind, ref] = subject ? subject.split(':') : ['', ''];
        const subjectServerUuid = kind === 'server' ? ref : '';

        const res = await createTicket({
            categoryId: Number(categoryId),
            serverUuid: serverUuid || subjectServerUuid || undefined,
            subjectKind: (kind || undefined) as 'server' | 'node' | 'route' | undefined,
            subjectRef: kind === 'node' || kind === 'route' ? ref : undefined,
            title: trimmedTitle,
            firstMessage: trimmedBody,
            priority: (priority || undefined) as TicketPriority | undefined,
        });
        setSubmitting(false);
        if (res.success && res.ticket) {
            router.push(`/tickets/${res.ticket.id}`);
        } else {
            setError(res.message || 'Failed to create ticket.');
        }
    };

    // Deliberately first and always available: someone with nothing yet, or a
    // question about billing, still has to be able to file a ticket. The
    // group (server / machine / address) travels as the option badge.
    const subjectOptions: SelectOption[] = [
        { value: '', label: 'Nothing specific - a general question' },
        ...servers.map(sv => ({ value: `server:${sv.uuid}`, label: sv.name, badge: 'server' })),
        ...ownNodes.map(n => ({ value: `node:${n.id}`, label: n.label, badge: 'machine' })),
        ...ownRoutes.map(d => ({ value: `route:${d}`, label: d, badge: 'address' })),
    ];

    return (
        <div className="mx-auto w-full max-w-2xl space-y-6">
            <Link href="/tickets" className="tk-link-btn text-(--base-06)">
                <ArrowLeft size={12} /> Back to tickets
            </Link>
            <PageHeader
                title="New ticket"
                description="Pick a category, say which of your services it concerns, and describe what's going on."
            />

            {error && (
                <div className="srv-callout" data-tone="error" role="alert">
                    <CircleAlert size={14} className="mt-0.5 shrink-0 text-(--error-light)" />
                    <span>{error}</span>
                </div>
            )}

            <form onSubmit={handleSubmit} className="srv-surface space-y-5 p-6">
                <div className="tk-field">
                    <label className="input-label" htmlFor="ticket-category">Category</label>
                    <Select
                        id="ticket-category"
                        value={categoryId === '' ? '' : String(categoryId)}
                        onChange={v => setCategoryId(v ? Number(v) : '')}
                        options={categories.map(c => ({ value: String(c.id), label: c.name }))}
                        placeholder="Pick a category"
                        ariaLabel="Category"
                        disabled={loading || submitting}
                    />
                    {selectedCategory?.description && <p className="tk-hint">{selectedCategory.description}</p>}
                </div>

                <div className="tk-field">
                    <label className="input-label" htmlFor="ticket-subject">What is this about?</label>
                    <Select
                        id="ticket-subject"
                        value={subject}
                        onChange={setSubject}
                        options={subjectOptions}
                        ariaLabel="What is this about?"
                        disabled={submitting}
                    />
                    <p className="tk-hint">
                        Optional, and it saves a round trip: support sees straight away which of your
                        services the ticket is about.
                    </p>
                </div>

                {requiresServer && (
                    <div className="tk-field">
                        <label className="input-label" htmlFor="ticket-server">Server</label>
                        <Select
                            id="ticket-server"
                            value={serverUuid}
                            onChange={setServerUuid}
                            options={servers.map(s => ({ value: s.uuid, label: `${s.name} (${s.uuid.slice(0, 8)})` }))}
                            placeholder="Pick a server"
                            ariaLabel="Server"
                            disabled={submitting}
                        />
                        {servers.length === 0 && (
                            <p className="text-xs text-(--warning-light)">
                                You don&apos;t have any servers yet. Pick a different category, or create a server first.
                            </p>
                        )}
                    </div>
                )}

                <div className="tk-field">
                    <label className="input-label" htmlFor="ticket-title">Title</label>
                    <input
                        id="ticket-title"
                        type="text"
                        value={title}
                        onChange={e => setTitle(e.target.value)}
                        placeholder="Short summary of the issue"
                        maxLength={200}
                        minLength={3}
                        required
                        disabled={submitting}
                        className="input-field w-full"
                    />
                </div>

                <div className="tk-field">
                    <label className="input-label" htmlFor="ticket-body">Description</label>
                    <textarea
                        id="ticket-body"
                        value={firstMessage}
                        onChange={e => setFirstMessage(e.target.value)}
                        placeholder="Describe what's going on. Include any error messages, what you tried, and what you expected."
                        rows={8}
                        maxLength={10000}
                        required
                        disabled={submitting}
                        className="input-field w-full"
                    />
                    <p className="tk-hint srv-num">{firstMessage.length} / 10000</p>
                </div>

                {selectedCategory && (
                    <div className="tk-field">
                        <label className="input-label" htmlFor="ticket-priority">Priority</label>
                        <Select
                            id="ticket-priority"
                            value={priority}
                            onChange={v => setPriority(v as TicketPriority)}
                            options={PRIORITY_OPTIONS}
                            ariaLabel="Priority"
                            disabled={submitting}
                            className="w-full sm:w-48"
                        />
                    </div>
                )}

                <div className="flex justify-end gap-2 pt-1">
                    <Link href="/tickets" className="btn btn-secondary">Cancel</Link>
                    <button type="submit" disabled={submitting} className="btn btn-primary disabled:opacity-40">
                        {submitting && <Loader2 size={14} className="animate-spin" />}
                        Submit ticket
                    </button>
                </div>
            </form>
        </div>
    );
}
