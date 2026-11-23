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
import { StatusBadge, timeAgo } from '@/components/tickets/TicketBadges';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import Select from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import Tooltip from '@/components/ui/Tooltip';
import { RowList, Row } from '@/components/ui/RowList';
import { Popover, PopoverContent, PopoverTrigger } from '@ui-sdk/react/popover';
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

    if (!featureFlags.tickets) return <TicketsDisabledBanner />;

    if (loading) return (
        <div className="space-y-6">
            <SkeletonText width="w-24" />
            <SkeletonHeader />
            <div className="space-y-3">
                <SkeletonCard height="h-20" className="max-w-2xl" />
                <SkeletonCard height="h-24" className="ml-auto max-w-2xl" />
                <SkeletonCard height="h-16" className="max-w-2xl" />
            </div>
            <Skeleton className="h-28 w-full rounded-lg" />
        </div>
    );
    if (!ticket) {
        return (
            <EmptyState
                tone="error"
                icon={<LifeBuoy size={20} />}
                title={error || 'Ticket not found.'}
                action={<Link href="/tickets" className="btn btn-secondary"><ArrowLeft size={14} /> Back to tickets</Link>}
                className="mx-auto mt-8 max-w-md"
            />
        );
    }

    const subjectChip = 'srv-chip inline-flex';

    return (
        <div className="space-y-6">
            <Link href={isSupport ? '/tickets/inbox' : '/tickets'} className="tk-link-btn text-(--base-06)">
                <ArrowLeft size={12} /> Back
            </Link>

            <PageHeader
                title={ticket.title}
                description={<>
                    <span className="srv-num">#{ticket.id}</span> · {ticket.categoryName} · opened by {ticket.username}
                </>}
                actions={<>
                    {canMutate ? (
                        <Select
                            value={ticket.status}
                            onChange={v => handleStatus(v as TicketStatus)}
                            options={ALL_STATUSES.map(s => ({ value: s, label: s.replace('_', ' ') }))}
                            ariaLabel="Ticket status"
                            className="w-40"
                        />
                    ) : (
                        <StatusBadge status={ticket.status} />
                    )}
                    {isOwner && !canMutate && ticket.status !== 'closed' && (
                        <button type="button" onClick={() => handleStatus('closed')} className="btn btn-secondary btn-sm">
                            <CircleCheckBig size={12} /> Close
                        </button>
                    )}
                    {user?.isAdmin && (
                        ticketSettings && !ticketSettings.deletionEnabled ? (
                            <Tooltip content="Ticket deletion is disabled in Settings → Ticket Settings.">
                                {/* aria-disabled keeps the button focusable so the tooltip can show */}
                                <button type="button" aria-disabled="true" className="btn btn-danger btn-sm cursor-not-allowed opacity-50">
                                    <Trash2 size={12} /> Delete
                                </button>
                            </Tooltip>
                        ) : (
                            <button type="button" onClick={() => setConfirmDelete(true)} className="btn btn-danger btn-sm">
                                <Trash2 size={12} /> Delete
                            </button>
                        )
                    )}
                </>}
            />

            {/* The way into the attached server. This replaces the sidebar's
                "Via tickets" tab, which listed those servers beside the user's
                own and made a support grant look like ownership. Opens in a
                new tab so the ticket stays put. */}
            {(ticket.serverName || ((ticket.subjectKind === 'node' || ticket.subjectKind === 'route') && ticket.subjectRef) || canMutate) && (
                <div className="-mt-3 flex flex-wrap items-center gap-2">
                    {ticket.serverName && (
                        ticket.serverId ? (
                            <a href={`/servers/${ticket.serverId}`} target="_blank" rel="noopener noreferrer" className={`${subjectChip} text-(--accent-light)`}>
                                <ServerIcon size={12} />
                                {ticket.serverName}
                                <ExternalLink size={11} />
                            </a>
                        ) : (
                            <span className={subjectChip}>
                                <ServerIcon size={12} />
                                {ticket.serverName} (deleted)
                            </span>
                        )
                    )}
                    {/* A machine or an address is not a server and has no page
                        to open - naming it is the whole point, so support does
                        not have to infer it from the prose. */}
                    {(ticket.subjectKind === 'node' || ticket.subjectKind === 'route') && ticket.subjectRef && (
                        <span className={subjectChip}>
                            {ticket.subjectKind === 'node' ? <HardDrive size={12} /> : <Globe size={12} />}
                            {ticket.subjectRef}
                        </span>
                    )}
                    {canMutate && (
                        ticket.assignedName ? (
                            <span className="flex items-center gap-2 text-xs text-(--base-07)">
                                Assigned to <span className="srv-num text-(--accent-light)">{ticket.assignedName}</span>
                                <button type="button" onClick={handleUnassign} className="tk-link-btn" data-danger="true">Unassign</button>
                            </span>
                        ) : (
                            <button type="button" onClick={handleSelfAssign} className="tk-link-btn">Assign to me</button>
                        )
                    )}
                </div>
            )}

            {error && (
                <div className="srv-callout" data-tone="error" role="alert">
                    <CircleAlert size={14} className="mt-0.5 shrink-0 text-(--error-light)" />
                    <span>{error}</span>
                </div>
            )}

            <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_288px]">
                <div className="min-w-0 space-y-6">
                    <div className="space-y-4">
                        {messages.map(m => (
                            <MessageBubble key={m.id} m={m} currentUserId={user?.id} />
                        ))}
                        {messages.length === 0 && (
                            <p className="text-sm text-(--base-06)">No messages yet.</p>
                        )}
                    </div>

                    {attachments.length > 0 && (
                        <section className="max-w-2xl">
                            <h2 className="srv-label mb-2 flex items-center gap-1.5">
                                <Paperclip size={12} /> Attachments ({attachments.length})
                            </h2>
                            <div className="srv-surface">
                                <RowList label="Attachments">
                                    {attachments.map(a => (
                                        <Row
                                            key={a.id}
                                            leading={<Paperclip size={14} />}
                                            title={a.filename}
                                            meta={<span className="srv-num">{formatBytes(a.sizeBytes)}{a.username ? ` · ${a.username}` : ''}</span>}
                                            actions={<>
                                                <Tooltip content="Download">
                                                    <a href={downloadTicketAttachmentURL(ticketId, a.id)} download={a.filename} aria-label={`Download ${a.filename}`} className="app-icon-btn">
                                                        <Download size={14} />
                                                    </a>
                                                </Tooltip>
                                                {(isSupport || a.uploadedBy === user?.id) && (
                                                    <Tooltip content="Delete">
                                                        <button type="button" onClick={() => handleDeleteAttachment(a.id)} aria-label={`Delete ${a.filename}`} className="app-icon-btn text-(--error-light)">
                                                            <Trash2 size={14} />
                                                        </button>
                                                    </Tooltip>
                                                )}
                                            </>}
                                        />
                                    ))}
                                </RowList>
                            </div>
                        </section>
                    )}

                    {ticket.status !== 'closed' ? (
                        <form onSubmit={handleReply} className="srv-surface space-y-3 p-4">
                            <textarea
                                value={replyBody}
                                onChange={e => setReplyBody(e.target.value)}
                                rows={3}
                                placeholder="Type your reply…"
                                aria-label="Reply"
                                maxLength={10000}
                                className="input-field w-full text-sm"
                                disabled={sending}
                            />
                            <div className="flex flex-wrap items-center justify-between gap-3">
                                <div className="flex flex-wrap items-center gap-4">
                                    {isSupport && (
                                        <Checkbox
                                            checked={replyIsInternal}
                                            onChange={setReplyIsInternal}
                                            label={<span className="inline-flex items-center gap-1.5"><Lock size={11} /> Internal note</span>}
                                        />
                                    )}
                                    {/* Support-only, category-scoped canned responses. */}
                                    {isSupport && cannedResponses.length > 0 && (
                                        <Popover open={showCannedMenu} onOpenChange={setShowCannedMenu}>
                                            <PopoverTrigger render={<button type="button" />} className="tk-link-btn" aria-label="Insert canned response">
                                                <MessageSquareQuote size={12} /> Templates ({cannedResponses.length})
                                            </PopoverTrigger>
                                            <PopoverContent side="top" align="start" sideOffset={8} aria-label="Canned responses" className="ui-popup z-40 max-h-72 w-80 overflow-y-auto p-1.5">
                                                {cannedResponses.map(c => (
                                                    <button
                                                        key={c.id}
                                                        type="button"
                                                        onClick={() => handleInsertCanned(c)}
                                                        className="dropdown-item w-full flex-col items-start text-left"
                                                    >
                                                        <span className="text-sm font-medium text-(--base-09)">{c.name}</span>
                                                        <span className="mt-0.5 line-clamp-2 text-xs text-(--base-06)">{c.body}</span>
                                                    </button>
                                                ))}
                                            </PopoverContent>
                                        </Popover>
                                    )}
                                    <Tooltip content="Attach file">
                                        <button
                                            type="button"
                                            onClick={() => fileInputRef.current?.click()}
                                            disabled={uploading}
                                            className="tk-link-btn"
                                        >
                                            {uploading ? <Loader2 size={12} className="animate-spin" /> : <Paperclip size={12} />}
                                            {uploading ? 'Uploading…' : 'Attach'}
                                        </button>
                                    </Tooltip>
                                    <input
                                        ref={fileInputRef}
                                        type="file"
                                        className="hidden"
                                        aria-label="Attach file"
                                        onChange={e => {
                                            const file = e.target.files?.[0];
                                            if (file) handleUpload(file);
                                            // Reset so picking the same file twice still triggers change.
                                            if (fileInputRef.current) fileInputRef.current.value = '';
                                        }}
                                    />
                                </div>
                                <button type="submit" disabled={sending || !replyBody.trim()} className="btn btn-primary disabled:opacity-40">
                                    {sending ? <Loader2 size={14} className="animate-spin" /> : <Send size={14} />}
                                    Send
                                </button>
                            </div>
                        </form>
                    ) : (
                        <div className="srv-callout" data-tone="info">
                            This ticket is closed. Reopen it to add new replies.
                        </div>
                    )}
                </div>

                {/* Shown on every width now; it used to vanish below lg. */}
                <aside className="min-w-0 space-y-6">
                    <section className="space-y-2">
                        <div className="flex items-center justify-between">
                            <h2 className="srv-label flex items-center gap-1.5"><UserPlus size={12} /> Watchers</h2>
                            {!showAddWatcher && (
                                <button type="button" onClick={() => setShowAddWatcher(true)} className="tk-link-btn">
                                    <UserPlus size={12} /> Add watcher
                                </button>
                            )}
                        </div>
                        <div className="srv-surface">
                            {watchers.length === 0 ? (
                                <p className="px-4 py-3 text-xs text-(--base-06)">No one is CC&apos;d.</p>
                            ) : (
                                <RowList label="Watchers">
                                    {watchers.map(w => (
                                        <Row
                                            key={w.userId}
                                            title={<span className="srv-num">{w.username}</span>}
                                            trailing={w.canReply ? <span className="text-(--accent-light)">reply</span> : undefined}
                                            actions={
                                                <Tooltip content="Remove">
                                                    <button type="button" onClick={() => handleRemoveWatcher(w.userId)} aria-label={`Remove ${w.username}`} className="app-icon-btn text-(--error-light)">
                                                        <XIcon size={14} />
                                                    </button>
                                                </Tooltip>
                                            }
                                        />
                                    ))}
                                </RowList>
                            )}
                            {showAddWatcher && (
                                <form onSubmit={handleAddWatcher} className="space-y-2 p-3">
                                    <input
                                        type="text"
                                        value={newWatcherName}
                                        onChange={e => setNewWatcherName(e.target.value)}
                                        placeholder="Username"
                                        aria-label="Watcher username"
                                        autoFocus
                                        className="input-field w-full text-xs py-1"
                                    />
                                    <div className="flex gap-2">
                                        <button type="submit" disabled={addingWatcher} className="btn btn-primary btn-sm flex-1">
                                            {addingWatcher ? 'Adding…' : 'Add'}
                                        </button>
                                        <button type="button" onClick={() => setShowAddWatcher(false)} className="btn btn-secondary btn-sm">
                                            Cancel
                                        </button>
                                    </div>
                                </form>
                            )}
                        </div>
                    </section>

                    {isSupport && audit.length > 0 && (
                        <section className="space-y-2">
                            <button
                                type="button"
                                onClick={() => setShowAudit(s => !s)}
                                aria-expanded={showAudit}
                                className="srv-label flex items-center gap-1.5 hover:text-(--base-09)"
                            >
                                <History size={12} /> Audit ({audit.length})
                            </button>
                            {showAudit && (
                                <ul className="srv-surface space-y-2 p-4 text-xs text-(--base-06)">
                                    {audit.map(e => (
                                        <li key={e.id}>
                                            <span className="srv-num text-(--base-07)">{e.eventType}</span>
                                            {e.actorName && <> by <span className="srv-num">{e.actorName}</span></>}
                                            <span> · {timeAgo(e.createdAt)}</span>
                                        </li>
                                    ))}
                                </ul>
                            )}
                        </section>
                    )}
                </aside>
            </div>

            {/* Admin-only confirm-delete modal. */}
            {confirmDelete && user?.isAdmin && (
                <div className="modal-overlay">
                    <ModalPanel onClose={() => setConfirmDelete(false)} className="modal-panel max-w-md">
                        <div className="modal-header">
                            <h2 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <Trash2 size={16} /> Delete ticket?
                            </h2>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Ticket <span className="srv-num">#{ticket.id}</span> - <span className="text-(--base-09)">&ldquo;{ticket.title}&rdquo;</span> will be permanently deleted. This action is recorded in the audit log.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button type="button" onClick={() => setConfirmDelete(false)} disabled={deleting} className="btn btn-secondary">
                                Cancel
                            </button>
                            <button type="button" onClick={handleDelete} disabled={deleting} className="btn btn-danger disabled:opacity-50">
                                {deleting ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                {deleting ? 'Deleting...' : 'Delete permanently'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </div>
    );
}

function MessageBubble({ m, currentUserId }: { m: TicketMessage; currentUserId?: string }) {
    const isMine = m.userId === currentUserId;
    const staff = m.userRole === 'support' || m.userRole === 'admin';
    return (
        <div className={`max-w-2xl ${isMine ? 'ml-auto' : ''}`}>
            <div className="mb-1 flex items-center gap-2 px-1">
                <span className={`text-xs font-medium ${staff ? 'text-(--accent-light)' : 'text-(--base-08)'}`}>{m.username || 'unknown'}</span>
                {m.userRole && m.userRole !== 'user' && (
                    <span className="tk-pill capitalize" data-tone="accent">{m.userRole}</span>
                )}
                <span className="text-xs text-(--base-06)">{timeAgo(m.createdAt)}</span>
            </div>
            <div className="tk-msg" data-mine={isMine && !m.isInternal} data-internal={m.isInternal}>
                {m.isInternal && (
                    <p className="mb-1.5 flex items-center gap-1 text-xs text-(--warning-light)">
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
