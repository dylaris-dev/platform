"use client";

import React, { useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import Link from 'next/link';
import { ArrowLeft, LifeBuoy, Loader2, CircleAlert } from 'lucide-react';
import { listTicketCategories, createTicket, TicketCategory, TicketPriority } from '@/lib/api/tickets';
import { useAppData } from '@/lib/AppDataContext';
import TicketsDisabledBanner from '@/components/tickets/TicketsDisabledBanner';
import { getNodes, getLinkRoutes } from '@/lib/api';

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

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    const selectedCategory = categories.find(c => c.id === categoryId);
    const requiresServer = !!selectedCategory?.requiresServer;

    // When the category changes, snap the priority back to its default so
    // the user can opt to override only when they actually mean to.
    useEffect(() => {
        if (selectedCategory) {
            setPriority(selectedCategory.defaultPriority);
        }
    }, [selectedCategory]);

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

    return (
        <main className="flex-1 flex flex-col overflow-y-auto p-6">
            <div className="max-w-2xl mx-auto w-full">
                <Link href="/tickets" className="inline-flex items-center gap-1.5 text-xs text-(--base-06) hover:text-(--accent-light) mb-3">
                    <ArrowLeft size={12} /> Back to tickets
                </Link>
                <h1 className="h-page flex items-center gap-2 mb-2">
                    <LifeBuoy size={22} className="text-(--accent-light)" />
                    New ticket
                </h1>
                <p className="text-sm text-(--base-06) mb-6">
                    Pick a category, say which of your services it concerns, and describe what&apos;s going on.
                </p>

                {error && (
                    <div className="alert alert-error mb-4 flex items-center gap-2">
                        <CircleAlert size={14} className="shrink-0" />
                        <span>{error}</span>
                    </div>
                )}

                <form onSubmit={handleSubmit} className="space-y-4">
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Category</label>
                        <select
                            value={categoryId}
                            onChange={e => setCategoryId(e.target.value ? Number(e.target.value) : '')}
                            disabled={loading || submitting}
                            required
                            className="input-field w-full"
                        >
                            <option value="">— Pick a category —</option>
                            {categories.map(c => (
                                <option key={c.id} value={c.id}>{c.name}</option>
                            ))}
                        </select>
                        {selectedCategory?.description && (
                            <p className="text-xs text-(--base-06) leading-snug mt-1">{selectedCategory.description}</p>
                        )}
                    </div>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="ticket-subject">What is this about?</label>
                        <select
                            id="ticket-subject"
                            value={subject}
                            onChange={e => setSubject(e.target.value)}
                            disabled={submitting}
                            className="input-field w-full"
                        >
                            {/* Deliberately first and always available: someone
                                with nothing yet, or a question about billing,
                                still has to be able to file a ticket. */}
                            <option value="">Nothing specific — a general question</option>
                            {servers.length > 0 && (
                                <optgroup label="Servers">
                                    {servers.map(sv => (
                                        <option key={sv.uuid} value={`server:${sv.uuid}`}>{sv.name}</option>
                                    ))}
                                </optgroup>
                            )}
                            {ownNodes.length > 0 && (
                                <optgroup label="My machines">
                                    {ownNodes.map(n => (
                                        <option key={n.id} value={`node:${n.id}`}>{n.label}</option>
                                    ))}
                                </optgroup>
                            )}
                            {ownRoutes.length > 0 && (
                                <optgroup label="Protected addresses">
                                    {ownRoutes.map(d => (
                                        <option key={d} value={`route:${d}`}>{d}</option>
                                    ))}
                                </optgroup>
                            )}
                        </select>
                        <p className="text-xs text-(--base-06) leading-snug">
                            Optional, and it saves a round trip: support sees straight away which of your
                            services the ticket is about.
                        </p>
                    </div>

                    {requiresServer && (
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Server</label>
                            <select
                                value={serverUuid}
                                onChange={e => setServerUuid(e.target.value)}
                                disabled={submitting}
                                required
                                className="input-field w-full"
                            >
                                <option value="">— Pick a server —</option>
                                {servers.map(s => (
                                    <option key={s.uuid} value={s.uuid}>{s.name} ({s.uuid.slice(0, 8)})</option>
                                ))}
                            </select>
                            {servers.length === 0 && (
                                <p className="text-xs text-(--warning-light)">
                                    You don&apos;t have any servers yet. Pick a different category, or create a server first.
                                </p>
                            )}
                        </div>
                    )}

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Title</label>
                        <input
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

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Description</label>
                        <textarea
                            value={firstMessage}
                            onChange={e => setFirstMessage(e.target.value)}
                            placeholder="Describe what's going on. Include any error messages, what you tried, and what you expected."
                            rows={8}
                            maxLength={10000}
                            required
                            disabled={submitting}
                            className="input-field w-full"
                        />
                        <p className="text-xs text-(--base-06)">{firstMessage.length} / 10000</p>
                    </div>

                    {selectedCategory && (
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Priority</label>
                            <select
                                value={priority}
                                onChange={e => setPriority(e.target.value as TicketPriority)}
                                disabled={submitting}
                                className="input-field w-full sm:w-48"
                            >
                                <option value="low">Low</option>
                                <option value="normal">Normal</option>
                                <option value="high">High</option>
                                <option value="urgent">Urgent</option>
                            </select>
                        </div>
                    )}

                    <div className="flex justify-end gap-2 pt-2">
                        <Link href="/tickets" className="btn btn-secondary">Cancel</Link>
                        <button
                            type="submit"
                            disabled={submitting}
                            className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40"
                        >
                            {submitting && <Loader2 size={14} className="animate-spin" />}
                            Submit ticket
                        </button>
                    </div>
                </form>
            </div>
        </main>
    );
}
