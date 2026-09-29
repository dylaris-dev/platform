"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';

import {
    ArrowLeft, Send, Loader2, LifeBuoy, Lock, UserPlus, X as XIcon, History, CircleAlert, CircleCheckBig,
    Paperclip, Download, Trash2, MessageSquareQuote, Server as ServerIcon, ExternalLink,
    HardDrive, Globe,
} from 'lucide-react';
import {
    getTicket, addTicketReply, setTicketStatus, setTicketAssignment,
    addTicketWatcher, removeTicketWatcher,
    listTicketAttachments, uploadTicketAttachment, deleteTicketAttachment, downloadTicketAttachmentURL,
    listCannedResponses, expandCannedTemplate,
    deleteTicket, getTicketSettings,
    Ticket, TicketMessage, TicketWatcher, TicketAuditEvent, TicketStatus, TicketAttachment, CannedResponse,
    TicketSettings,
} from '@/lib/api/tickets';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonHeader, SkeletonText, SkeletonCard, Skeleton } from '@/components/Skeleton';
import TicketsDisabledBanner from '@/components/tickets/TicketsDisabledBanner';
import { useRouter } from 'next/navigation';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';

const ALL_STATUSES: TicketStatus[] = ['open', 'in_progress', 'waiting_user', 'resolved', 'closed'];

export default function TicketDetailPage() {
    const paramId = useRouteId('tickets');
    const router = useRouter();
    const ticketId = Number(paramId);
    const { user, featureFlags } = useAppData();
    const [ticketSettings, setTicketSettings] = useState<TicketSettings | null>(null);
    const [confirmDelete, setConfirmDelete] = useState(false);
    const [deleting, setDeleting] = useState(false);

    const [ticket, setTicket] = useState<Ticket | null>(null);
    const [messages, setMessages] = useState<TicketMessage[]>([]);
    const [watchers, setWatchers] = useState<TicketWatcher[]>([]);
    const [audit, setAudit] = useState<TicketAuditEvent[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');

    const [replyBody, setReplyBody] = useState('');
    const [replyIsInternal, setReplyIsInternal] = useState(false);
    const [sending, setSending] = useState(false);

    // attachments + canned responses
    const [attachments, setAttachments] = useState<TicketAttachment[]>([]);
    const [uploading, setUploading] = useState(false);
    const fileInputRef = React.useRef<HTMLInputElement | null>(null);
    const [cannedResponses, setCannedResponses] = useState<CannedResponse[]>([]);
    const [showCannedMenu, setShowCannedMenu] = useState(false);

    const [showAudit, setShowAudit] = useState(false);
    const [showAddWatcher, setShowAddWatcher] = useState(false);
    const [newWatcherName, setNewWatcherName] = useState('');
    const [addingWatcher, setAddingWatcher] = useState(false);

    const isSupport = user?.role === 'support' || user?.isAdmin;
    const isOwner = ticket?.userId === user?.id;
    const canMutate = !!isSupport;

    const reload = useCallback(async () => {
        const res = await getTicket(ticketId);
        if (res.success && res.ticket) {
            setTicket(res.ticket);
            setMessages(res.messages || []);
            setWatchers(res.watchers || []);
            setAudit(res.audit || []);
        } else {
            setError(res.message || 'Ticket not found.');
        }
        setLoading(false);
    }, [ticketId]);

    useEffect(() => {
        if (!Number.isFinite(ticketId) || ticketId <= 0) return;
        if (!featureFlags.tickets) return;
        reload();
        // Attachments live on a separate endpoint so we can reload them
        // independently after an upload without re-fetching the whole ticket.
        listTicketAttachments(ticketId).then(r => {
            if (r.success) setAttachments(r.attachments || []);
        });
    }, [ticketId, reload, featureFlags.tickets]);

    // Admins need ticketSettings.deletionEnabled to know whether to show the
    // Delete button enabled or disabled-with-tooltip. The endpoint is admin-
    // only so we silently skip for non-admins.
    useEffect(() => {
        if (!user?.isAdmin || !featureFlags.tickets) return;
        getTicketSettings().then(res => {
            if (res.success && res.settings) setTicketSettings(res.settings);
        });
    }, [user?.isAdmin, featureFlags.tickets]);

    const handleDelete = async () => {
        setDeleting(true);
        const res = await deleteTicket(ticketId);
        setDeleting(false);
        if (res.success) {
            router.replace(isSupport ? '/tickets/inbox' : '/tickets');
        } else {
            setError(res.message || 'Deletion failed.');
            setConfirmDelete(false);
        }
    };

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    // Canned responses are support-only and category-scoped. Re-fetch when
    // the ticket's category changes so we get the right snippets.
    useEffect(() => {
        if (!isSupport || !ticket) return;
        listCannedResponses(ticket.categoryId).then(r => {
            if (r.success) setCannedResponses(r.responses || []);
        });
    }, [isSupport, ticket?.categoryId]);

    const handleReply = async (e: React.FormEvent) => {
        e.preventDefault();
        const body = replyBody.trim();
        if (!body) return;
        setSending(true);
        const res = await addTicketReply(ticketId, body, replyIsInternal);
        setSending(false);
        if (res.success) {
            setReplyBody('');
            setReplyIsInternal(false);
            reload();
        } else {
            setError(res.message || 'Reply failed.');
        }
    };

    const handleStatus = async (status: TicketStatus) => {
        const res = await setTicketStatus(ticketId, status);
        if (res.success) reload();
        else setError(res.message || 'Status update failed.');
    };

    const handleSelfAssign = async () => {
        if (!user) return;
        const res = await setTicketAssignment(ticketId, { assignedUserId: user.id, assignedTeam: ticket?.assignedTeam || '' });
        if (res.success) reload();
        else setError(res.message || 'Assignment failed.');
    };

    const handleUnassign = async () => {
        const res = await setTicketAssignment(ticketId, { assignedUserId: null, assignedTeam: ticket?.assignedTeam || '' });
        if (res.success) reload();
        else setError(res.message || 'Assignment failed.');
    };

    const handleAddWatcher = async (e: React.FormEvent) => {
        e.preventDefault();
        const username = newWatcherName.trim();
        if (!username) return;
        setAddingWatcher(true);
        const res = await addTicketWatcher(ticketId, { username });
        setAddingWatcher(false);
        if (res.success) {
            setNewWatcherName('');
            setShowAddWatcher(false);
            reload();
        } else {
            setError(res.message || 'Failed to add watcher.');
        }
    };

    const handleRemoveWatcher = async (userId: string) => {
        const res = await removeTicketWatcher(ticketId, userId);
        if (res.success) {
            reload();
            return;
        }
        // A failure here used to leave the watcher on screen with nothing said,
        // which reads as a click that did not register. Core now answers 404
        // when the removal matched no row - the list on screen is simply stale -
        // so reload either way and do not repeat its wording, which is
        // deliberately the same as for a ticket that does not exist.
        setError('Could not remove this watcher. The list has been refreshed.');
        reload();
    };

    const handleUpload = async (file: File) => {
        setUploading(true);
        const res = await uploadTicketAttachment(ticketId, file);
        setUploading(false);
        if (res.success) {
            const r = await listTicketAttachments(ticketId);
            if (r.success) setAttachments(r.attachments || []);
        } else {
            setError(res.message || 'Upload failed.');
        }
    };

    const handleDeleteAttachment = async (attachmentId: number) => {
        if (!(await confirmDialog({ title: 'Delete attachment', message: 'Delete this attachment?' }))) return;
        const res = await deleteTicketAttachment(ticketId, attachmentId);
        if (res.success) {
            setAttachments(prev => prev.filter(a => a.id !== attachmentId));
        } else {
            setError(res.message || 'Deleting the attachment failed.');
        }
    };

    const handleInsertCanned = (c: CannedResponse) => {
        const expanded = expandCannedTemplate(c.body, {
            userName: ticket?.username,
            ticketId: ticket?.id,
            serverName: ticket?.serverName,
            actorName: user?.username,
        });
        setReplyBody(prev => prev ? prev + '\n\n' + expanded : expanded);
        setShowCannedMenu(false);
    };

    if (loading) return (
        <main className="flex-1 flex flex-col overflow-hidden">
            <div className="shrink-0 border-b border-(--base-03) p-6 pb-4">
                <SkeletonText width="w-24" className="mb-3" />
                <SkeletonHeader />
            </div>
            <div className="flex-1 overflow-y-auto p-6 space-y-3">
                <SkeletonCard height="h-20" className="max-w-2xl" />
                <SkeletonCard height="h-24" className="max-w-2xl ml-auto" />
                <SkeletonCard height="h-16" className="max-w-2xl" />
            </div>
            <div className="shrink-0 border-t border-(--base-03) p-4 bg-(--base-02)">
                <Skeleton className="h-20 w-full rounded" />
            </div>
        </main>
    );
    if (!ticket) {
        return (
            <main className="flex-1 flex items-center justify-center p-6 text-(--base-06)">
                <div className="text-center max-w-md">
                    <LifeBuoy size={32} className="mx-auto" />
                    <p className="mt-3 text-sm">{error || 'Ticket not found.'}</p>
                    <Link href="/tickets" className="btn btn-secondary mt-4 inline-flex items-center gap-2">
                        <ArrowLeft size={14} /> Back to tickets
                    </Link>
                </div>
            </main>
        );
    }

    return (
        <main className="flex-1 flex flex-col overflow-hidden">
            {/* Header */}
            <div className="shrink-0 border-b border-(--base-03) p-6 pb-4">
                <Link href={isSupport ? '/tickets/inbox' : '/tickets'} className="inline-flex items-center gap-1.5 text-xs text-(--base-06) hover:text-(--accent-light) mb-3">
                    <ArrowLeft size={12} /> Back
                </Link>
                <div className="flex items-start justify-between gap-4 flex-wrap">
                    <div className="min-w-0">
                        <h1 className="text-xl font-display text-(--base-09)">{ticket.title}</h1>
                        <p className="text-xs text-(--base-06) mt-1 font-mono">
                            #{ticket.id} · {ticket.categoryName} · opened by {ticket.username}
                        </p>
                        {/* The way into the attached server. This replaces the
                            sidebar's "Via tickets" tab, which listed those servers
                            beside the user's own and made a support grant look like
                            ownership. Opens in a new tab so the ticket stays put. */}
                        {ticket.serverName && (
                            ticket.serverId ? (
                                <a
                                    href={`/servers/${ticket.serverId}`}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    className="inline-flex items-center gap-1.5 mt-2 px-2 py-1 rounded-md bg-(--accent-ghost) border border-(--accent-border) text-xs text-(--accent-light) hover:bg-(--accent)/15 transition-colors"
                                >
                                    <ServerIcon size={12} />
                                    {ticket.serverName}
                                    <ExternalLink size={11} />
                                </a>
                            ) : (
                                <span className="inline-flex items-center gap-1.5 mt-2 px-2 py-1 rounded-md bg-(--base-03) border border-(--base-04) text-xs text-(--base-06)">
                                    <ServerIcon size={12} />
                                    {ticket.serverName} (deleted)
                                </span>
                            )
                        )}
                        {/* A machine or an address is not a server and has no
                            page to open - naming it is the whole point, so
                            support does not have to infer it from the prose. */}
                        {(ticket.subjectKind === 'node' || ticket.subjectKind === 'route') && ticket.subjectRef && (
                            <span className="inline-flex items-center gap-1.5 mt-2 px-2 py-1 rounded-md bg-(--base-03) border border-(--base-04) text-xs text-(--base-07)">
                                {ticket.subjectKind === 'node' ? <HardDrive size={12} /> : <Globe size={12} />}
                                {ticket.subjectRef}
                            </span>
                        )}
                    </div>
                    <div className="flex items-center gap-2 flex-wrap">
                        {canMutate ? (
                            <select
                                value={ticket.status}
                                onChange={e => handleStatus(e.target.value as TicketStatus)}
                                className="input-field text-sm py-1.5"
                            >
                                {ALL_STATUSES.map(s => <option key={s} value={s}>{s.replace('_', ' ')}</option>)}
                            </select>
                        ) : (
                            <span className="text-xs font-mono uppercase tracking-[0.06em] px-2 py-1 rounded-md bg-(--base-03) text-(--base-07)">
                                {ticket.status.replace('_', ' ')}
                            </span>
                        )}
                        {isOwner && !canMutate && ticket.status !== 'closed' && (
                            <button
                                type="button"
                                onClick={() => handleStatus('closed')}
                                className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                            >
                                <CircleCheckBig size={12} /> Close
                            </button>
                        )}
                        {user?.isAdmin && (
                            ticketSettings && !ticketSettings.deletionEnabled ? (
                                <button
                                    type="button"
                                    disabled
                                    title="Ticket deletion is disabled in Settings → Ticket Settings."
                                    className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 opacity-50 cursor-not-allowed"
                                >
                                    <Trash2 size={12} /> Delete
                                </button>
                            ) : (
                                <button
                                    type="button"
                                    onClick={() => setConfirmDelete(true)}
                                    className="btn btn-sm inline-flex items-center gap-1.5 bg-(--error-ghost) text-(--error-light) border border-(--error)/30 hover:bg-(--error)/15"
                                    title="Delete ticket"
                                >
                                    <Trash2 size={12} /> Delete
                                </button>
                            )
                        )}
                    </div>
                </div>
                {canMutate && (
                    <div className="flex items-center gap-2 mt-3 text-xs text-(--base-07)">
                        {ticket.assignedName ? (
                            <span className="flex items-center gap-1">
                                Assigned to <span className="font-mono text-(--accent-light)">{ticket.assignedName}</span>
                                <button type="button" onClick={handleUnassign} className="text-(--error-light) hover:underline ml-1">unassign</button>
                            </span>
                        ) : (
                            <button type="button" onClick={handleSelfAssign} className="text-(--accent-light) hover:underline">Assign to me</button>
                        )}
                    </div>
                )}
            </div>

            {/* Body — messages + sidebar */}
            <div className="flex-1 flex overflow-hidden min-h-0">
                <div className="flex-1 overflow-y-auto p-6 space-y-3">
                    {messages.map(m => (
                        <MessageBubble key={m.id} m={m} currentUserId={user?.id} />
                    ))}
                    {messages.length === 0 && (
                        <p className="text-sm text-(--base-06) italic">No messages yet.</p>
                    )}

                    {/* Attachments inline below the messages. Sits in the
                        scroll container so it gets pushed down with new replies. */}
                    {attachments.length > 0 && (
                        <div className="mt-6 pt-4 border-t border-(--base-03) max-w-2xl">
                            <h3 className="mono-label mb-2 flex items-center gap-1.5">
                                <Paperclip size={12} /> Attachments ({attachments.length})
                            </h3>
                            <ul className="space-y-1.5">
                                {attachments.map(a => (
                                    <li key={a.id} className="flex items-center justify-between gap-2 p-2 rounded-md bg-(--base-02) border border-(--base-03)">
                                        <div className="min-w-0 flex items-center gap-2">
                                            <Paperclip size={14} className="text-(--base-06) shrink-0" />
                                            <div className="min-w-0">
                                                <p className="text-sm truncate">{a.filename}</p>
                                                <p className="text-[10px] font-mono text-(--base-06)">
                                                    {formatBytes(a.sizeBytes)}{a.username ? ` · ${a.username}` : ''}
                                                </p>
                                            </div>
                                        </div>
                                        <div className="flex items-center gap-1">
                                            <a
                                                href={downloadTicketAttachmentURL(ticketId, a.id)}
                                                className="p-1.5 rounded text-(--accent-light) hover:bg-(--accent-ghost)"
                                                title="Download"
                                                download={a.filename}
                                            >
                                                <Download size={12} />
                                            </a>
                                            {(isSupport || a.uploadedBy === user?.id) && (
                                                <button
                                                    type="button"
                                                    onClick={() => handleDeleteAttachment(a.id)}
                                                    className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost)"
                                                    title="Delete"
                                                >
                                                    <Trash2 size={12} />
                                                </button>
                                            )}
                                        </div>
                                    </li>
                                ))}
                            </ul>
                        </div>
                    )}
                </div>

                <aside className="w-72 shrink-0 border-l border-(--base-03) p-4 overflow-y-auto space-y-4 hidden lg:block">
                    <section>
                        <h3 className="mono-label mb-2 flex items-center gap-1.5">
                            <UserPlus size={12} /> Watchers
                        </h3>
                        {watchers.length === 0 && (
                            <p className="text-xs text-(--base-06) italic">No one is CC&apos;d.</p>
                        )}
                        <ul className="space-y-1.5">
                            {watchers.map(w => (
                                <li key={w.userId} className="flex items-center justify-between gap-2 text-sm">
                                    <span className="truncate">
                                        <span className="font-mono">{w.username}</span>
                                        {w.canReply && <span className="text-[10px] text-(--accent-light) ml-1.5">reply</span>}
                                    </span>
                                    <button
                                        type="button"
                                        onClick={() => handleRemoveWatcher(w.userId)}
                                        className="text-(--error-light) hover:bg-(--error-ghost) p-0.5 rounded"
                                        title="Remove"
                                    >
                                        <XIcon size={12} />
                                    </button>
                                </li>
                            ))}
                        </ul>
                        {showAddWatcher ? (
                            <form onSubmit={handleAddWatcher} className="mt-2 space-y-1.5">
                                <input
                                    type="text"
                                    value={newWatcherName}
                                    onChange={e => setNewWatcherName(e.target.value)}
                                    placeholder="Username"
                                    autoFocus
                                    className="input-field w-full text-xs py-1"
                                />
                                <div className="flex gap-1.5">
                                    <button type="submit" disabled={addingWatcher} className="btn btn-primary btn-sm flex-1 text-xs">
                                        {addingWatcher ? 'Adding…' : 'Add'}
                                    </button>
                                    <button type="button" onClick={() => setShowAddWatcher(false)} className="btn btn-secondary btn-sm text-xs">
                                        Cancel
                                    </button>
                                </div>
                            </form>
                        ) : (
                            <button
                                type="button"
                                onClick={() => setShowAddWatcher(true)}
                                className="text-xs text-(--accent-light) hover:underline mt-2 inline-flex items-center gap-1"
                            >
                                <UserPlus size={12} /> Add watcher
                            </button>
                        )}
                    </section>

                    {isSupport && audit.length > 0 && (
                        <section>
                            <button
                                type="button"
                                onClick={() => setShowAudit(s => !s)}
                                className="mono-label flex items-center gap-1.5 hover:text-(--base-09)"
                            >
                                <History size={12} /> Audit ({audit.length})
                            </button>
                            {showAudit && (
                                <ul className="space-y-1.5 mt-2 text-xs text-(--base-06)">
                                    {audit.map(e => (
                                        <li key={e.id}>
                                            <span className="font-mono text-(--base-07)">{e.eventType}</span>
                                            {e.actorName && <> by <span className="font-mono">{e.actorName}</span></>}
                                            <span className="text-(--base-06)"> · {timeAgo(e.createdAt)}</span>
                                        </li>
                                    ))}
                                </ul>
                            )}
                        </section>
                    )}
                </aside>
            </div>

            {/* Reply box */}
            {ticket.status !== 'closed' && (
                <form onSubmit={handleReply} className="shrink-0 border-t border-(--base-03) p-4 bg-(--base-02) relative">
                    {error && (
                        <p className="text-xs text-(--error-light) mb-2 flex items-center gap-1">
                            <CircleAlert size={12} /> {error}
                        </p>
                    )}

                    {/* Support-only: canned-response picker. Floats above the
                        textarea so it doesn't shift layout when toggled. */}
                    {isSupport && showCannedMenu && cannedResponses.length > 0 && (
                        <div className="absolute left-4 right-4 -top-2 -translate-y-full bg-(--base-02) border border-(--base-03) rounded-md shadow-lg max-h-72 overflow-y-auto z-10">
                            {cannedResponses.map(c => (
                                <button
                                    key={c.id}
                                    type="button"
                                    onClick={() => handleInsertCanned(c)}
                                    className="w-full text-left px-3 py-2 hover:bg-(--base-03) border-b border-(--base-03) last:border-b-0"
                                >
                                    <p className="text-sm font-medium">{c.name}</p>
                                    <p className="text-xs text-(--base-06) line-clamp-2 mt-0.5">{c.body}</p>
                                </button>
                            ))}
                        </div>
                    )}

                    <textarea
                        value={replyBody}
                        onChange={e => setReplyBody(e.target.value)}
                        rows={3}
                        placeholder="Type your reply…"
                        maxLength={10000}
                        className="input-field w-full text-sm"
                        disabled={sending}
                    />
                    <div className="flex items-center justify-between gap-2 mt-2 flex-wrap">
                        <div className="flex items-center gap-3">
                            {isSupport && (
                                <label className="flex items-center gap-1.5 text-xs text-(--base-07) cursor-pointer">
                                    <input
                                        type="checkbox"
                                        checked={replyIsInternal}
                                        onChange={e => setReplyIsInternal(e.target.checked)}
                                        className="checkbox"
                                    />
                                    <Lock size={11} /> Internal note
                                </label>
                            )}
                            {isSupport && cannedResponses.length > 0 && (
                                <button
                                    type="button"
                                    onClick={() => setShowCannedMenu(s => !s)}
                                    className="text-xs text-(--accent-light) hover:underline inline-flex items-center gap-1"
                                    title="Insert canned response"
                                >
                                    <MessageSquareQuote size={12} /> Templates ({cannedResponses.length})
                                </button>
                            )}
                            <button
                                type="button"
                                onClick={() => fileInputRef.current?.click()}
                                disabled={uploading}
                                className="text-xs text-(--accent-light) hover:underline inline-flex items-center gap-1 disabled:opacity-40"
                                title="Attach file"
                            >
                                {uploading ? <Loader2 size={12} className="animate-spin" /> : <Paperclip size={12} />}
                                {uploading ? 'Uploading…' : 'Attach'}
                            </button>
                            <input
                                ref={fileInputRef}
                                type="file"
                                className="hidden"
                                onChange={e => {
                                    const file = e.target.files?.[0];
                                    if (file) handleUpload(file);
                                    // Reset so picking the same file twice still triggers change.
                                    if (fileInputRef.current) fileInputRef.current.value = '';
                                }}
                            />
                        </div>
                        <button
                            type="submit"
                            disabled={sending || !replyBody.trim()}
                            className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40"
                        >
                            {sending ? <Loader2 size={14} className="animate-spin" /> : <Send size={14} />}
                            Send
                        </button>
                    </div>
                </form>
            )}
            {ticket.status === 'closed' && (
                <div className="shrink-0 border-t border-(--base-03) p-4 text-sm text-(--base-06) text-center italic">
                    This ticket is closed. Reopen it to add new replies.
                </div>
            )}

            {/* Admin-only confirm-delete modal. Renders inside <main> so it
                inherits the layout context but uses a fixed overlay to sit
                above the rest of the page. */}
            {confirmDelete && user?.isAdmin && (
                <div className="fixed inset-0 z-50 flex items-center justify-center p-6 bg-(--base-00)/70 backdrop-blur-sm">
                    <ModalPanel className="card max-w-md w-full p-6 border border-(--base-03)">
                        <div className="flex items-start gap-3">
                            <div className="w-10 h-10 rounded-md bg-(--error-ghost) flex items-center justify-center shrink-0">
                                <Trash2 size={18} className="text-(--error-light)" />
                            </div>
                            <div className="min-w-0">
                                <h2 className="text-base font-display font-bold text-(--base-09)">Delete ticket?</h2>
                                <p className="text-sm text-(--base-06) mt-1">
                                    Ticket <span className="font-mono">#{ticket.id}</span> - <span className="text-(--base-09)">&ldquo;{ticket.title}&rdquo;</span> will be permanently deleted. This action is recorded in the audit log.
                                </p>
                            </div>
                        </div>
                        <div className="flex items-center justify-end gap-2 mt-5">
                            <button
                                type="button"
                                onClick={() => setConfirmDelete(false)}
                                disabled={deleting}
                                className="btn btn-secondary btn-sm"
                            >
                                Cancel
                            </button>
                            <button
                                type="button"
                                onClick={handleDelete}
                                disabled={deleting}
                                className="btn btn-sm inline-flex items-center gap-1.5 bg-(--error) text-white border border-(--error) hover:bg-(--error)/85 disabled:opacity-50"
                            >
                                {deleting ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                {deleting ? 'Deleting...' : 'Delete permanently'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </main>
    );
}

function MessageBubble({ m, currentUserId }: { m: TicketMessage; currentUserId?: string }) {
    const isMine = m.userId === currentUserId;
    const roleClass = m.userRole === 'support' || m.userRole === 'admin' ? 'text-(--accent-light)' : 'text-(--base-09)';
    return (
        <div className={`max-w-2xl ${isMine ? 'ml-auto' : ''}`}>
            <div className="flex items-baseline gap-2 mb-1 px-1">
                <span className={`text-xs font-mono ${roleClass}`}>{m.username || 'unknown'}</span>
                {m.userRole && m.userRole !== 'user' && (
                    <span className="text-[9px] font-mono uppercase tracking-[0.06em] px-1 py-0.5 rounded-sm bg-(--accent-ghost) text-(--accent-light)">
                        {m.userRole}
                    </span>
                )}
                <span className="text-xs text-(--base-06)">{timeAgo(m.createdAt)}</span>
            </div>
            <div className={`p-3 rounded-md border whitespace-pre-wrap text-sm ${
                m.isInternal
                    ? 'bg-(--warning-ghost) border-(--warning)/30 text-(--base-09)'
                    : isMine
                        ? 'bg-(--accent-ghost) border-(--accent-border) text-(--base-09)'
                        : 'bg-(--base-02) border-(--base-03) text-(--base-09)'
            }`}>
                {m.isInternal && (
                    <p className="text-xs text-(--warning-light) flex items-center gap-1 mb-1.5">
                        <Lock size={11} /> Internal note
                    </p>
                )}
                {m.body}
            </div>
        </div>
    );
}

function formatBytes(n: number): string {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
    return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB';
}

function timeAgo(iso: string): string {
    try {
        const diff = Date.now() - new Date(iso).getTime();
        const min = Math.floor(diff / 60000);
        if (min < 1) return 'just now';
        if (min < 60) return `${min}m ago`;
        const hr = Math.floor(min / 60);
        if (hr < 24) return `${hr}h ago`;
        const d = Math.floor(hr / 24);
        return `${d}d ago`;
    } catch { return ''; }
}
