"use client";

import { useState, useEffect, useRef, useCallback } from 'react';
import Link from 'next/link';
import { Bell, AlertTriangle, ExternalLink, Inbox, CheckCheck } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { API_URL, getGatewayBandwidthOverview } from '@/lib/api';
import { shouldWarnBeamRelayMissing } from '@/lib/beamRelayNotice';
import { listNotifications, getUnreadCount, markNotificationRead, markAllNotificationsRead, Notification as InboxNotification } from '@/lib/api/notifications';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface Notification {
    id: string;
    severity: 'warning' | 'error';
    title: string;
    message: string;
    href?: string;
    cta?: string;
}

// ---------------------------------------------------------------------------
// Checks — each returns a Notification when it has something to report,
// null when everything is fine. Add new checks as the platform grows.
// ---------------------------------------------------------------------------

async function checkBeamRelayMissing(ctx: CheckContext): Promise<Notification | null> {
    try {
        const res = await fetch(`${API_URL}/settings/beam`, {
        });
        if (!res.ok) return null;
        const data = await res.json();
        if (!data.success || !data.settings) return null;
        // The decision lives in shouldWarnBeamRelayMissing, with the two reasons
        // this check used to fire on healthy installs written down beside it.
        if (!shouldWarnBeamRelayMissing({
            enabled: data.settings.enabled !== false,
            relayAddress: data.settings.relayAddress,
            gatewayEnabled: ctx.gatewayEnabled,
        })) return null;
        return {
            id: 'beam-relay-missing',
            severity: 'warning',
            title: 'Beam relay address not set',
            message:
                'The Beam desktop app gets the relay address from Core after login. Without it, file transfers can\'t connect.',
            href: '/settings/beam',
            cta: 'Open Beam settings',
        };
    } catch {
        return null;
    }
}

async function checkGatewayBandwidth(ctx: CheckContext): Promise<Notification | null> {
    // Bandwidth budgets are a gateway concept: the alerts come from edges and
    // links, and without routing there are none to be over budget.
    if (!ctx.gatewayEnabled) return null;
    try {
        const ov = await getGatewayBandwidthOverview();
        if (!ov?.alerts?.length) return null;
        const n = ov.alerts.length;
        return {
            id: 'gwbw-threshold',
            severity: 'warning',
            title: n === 1
                ? 'A gateway component is over its bandwidth budget'
                : `${n} gateway components over their bandwidth budget`,
            message:
                'Sustained above 80% for the alert window. Open Infrastructure > Bandwidth to see which hosts and components.',
            href: '/infrastructure/nodes',
            cta: 'Open Infrastructure',
        };
    } catch {
        return null;
    }
}

// What every check gets to decide with, so a check never has to reach into
// context itself (they run outside React).
interface CheckContext {
    gatewayEnabled: boolean;
}

// All registered checks. Run in parallel; null results are dropped.
const CHECKS: Array<(ctx: CheckContext) => Promise<Notification | null>> = [
    checkBeamRelayMissing,
    checkGatewayBandwidth,
];

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export default function NotificationsDropdown() {
    const { user, gatewayEnabled } = useAppData();
    // The inbox is NOT gated on the ticket feature, though it used to be on
    // both sides: Core 503'd these endpoints and the bell skipped polling them.
    // The inbox started out ticket-driven and has since grown notifications
    // that have nothing to do with tickets - server.install_failed is the only
    // way an owner learns their server did not install - so switching tickets
    // off was silently switching those off too.
    const [open, setOpen] = useState(false);
    const [items, setItems] = useState<Notification[]>([]);
    const [inbox, setInbox] = useState<InboxNotification[]>([]);
    const [unread, setUnread] = useState(0);
    const wrapRef = useRef<HTMLDivElement>(null);

    const isAdmin = user?.isAdmin ?? false;

    // Refresh the admin "system" checks. Cheap, but only meaningful for admin.
    const refreshChecks = useCallback(async () => {
        if (!isAdmin) {
            setItems([]);
            return;
        }
        const results = await Promise.all(CHECKS.map(c => c({ gatewayEnabled })));
        setItems(results.filter((n): n is Notification => n !== null));
    }, [isAdmin, gatewayEnabled]);

    // Refresh user inbox + unread badge. Cheap unread-count endpoint runs on
    // the same interval; the full inbox is only fetched on open.
    const refreshUnread = useCallback(async () => {
        const res = await getUnreadCount();
        if (res.success) setUnread(res.unread || 0);
    }, []);

    const refreshInbox = useCallback(async () => {
        const res = await listNotifications(false, 20);
        if (res.success) setInbox(res.notifications || []);
    }, []);

    // Initial load + 30s poll for both signals.
    useEffect(() => {
        refreshChecks();
        refreshUnread();
        const t = setInterval(() => {
            refreshChecks();
            refreshUnread();
        }, 30000);
        return () => clearInterval(t);
    }, [refreshChecks, refreshUnread]);

    // Pull the full inbox when the dropdown opens — keeps cost low for
    // users who never click it.
    useEffect(() => {
        if (open) refreshInbox();
    }, [open, refreshInbox]);

    // Click-outside closes the dropdown — matches the profile dropdown's
    // behavior in the same navbar.
    useEffect(() => {
        const handler = (e: MouseEvent) => {
            if (!wrapRef.current?.contains(e.target as Node)) setOpen(false);
        };
        document.addEventListener('click', handler);
        return () => document.removeEventListener('click', handler);
    }, []);

    const handleClickItem = async (n: InboxNotification) => {
        if (!n.readAt) {
            await markNotificationRead(n.id);
            // Optimistic local update — saves a round-trip on the next poll.
            setInbox(prev => prev.map(x => x.id === n.id ? { ...x, readAt: new Date().toISOString() } : x));
            setUnread(u => Math.max(0, u - 1));
        }
        setOpen(false);
    };

    const handleMarkAllRead = async () => {
        await markAllNotificationsRead();
        const now = new Date().toISOString();
        setInbox(prev => prev.map(n => n.readAt ? n : { ...n, readAt: now }));
        setUnread(0);
    };

    // Total bell-badge count is system-checks (admins only) + unread inbox.
    const badgeCount = items.length + unread;
    const hasItems = badgeCount > 0;
    const hasInbox = inbox.length > 0;
    const hasChecks = items.length > 0;

    return (
        <div ref={wrapRef} className="relative mr-2">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                title={hasItems ? `${badgeCount} unread` : 'No notifications'}
                className={`relative flex items-center justify-center w-9 h-9 rounded-md transition-colors border ${
                    open
                        ? 'bg-(--base-03) border-(--base-04) text-(--base-09)'
                        : hasItems
                            ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border) hover:bg-(--accent-ghost)/80'
                            : 'text-(--base-07) hover:bg-(--base-04)/50 hover:text-(--base-09) border-transparent'
                }`}
            >
                <Bell size={18} />
                {hasItems && (
                    <span className="absolute -top-1 -right-1 min-w-[16px] h-[16px] px-1 rounded-full bg-(--accent-light) text-(--base-00) text-[10px] font-mono font-bold flex items-center justify-center leading-none">
                        {badgeCount}
                    </span>
                )}
            </button>

            {open && (
                <div className="dropdown-menu right-0 mt-3 w-96 animate-fade-in origin-top-right">
                    <div className="px-4 py-2 border-b border-(--base-03) flex items-center justify-between">
                        <div className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                            Notifications
                        </div>
                        {unread > 0 && (
                            <button
                                type="button"
                                onClick={handleMarkAllRead}
                                className="text-xs text-(--accent-light) hover:underline inline-flex items-center gap-1"
                                title="Mark all as read"
                            >
                                <CheckCheck size={12} /> Mark all read
                            </button>
                        )}
                    </div>

                    {/* System checks — admin only */}
                    {isAdmin && hasChecks && (
                        <div className="border-b border-(--base-03)">
                            <div className="px-4 py-1.5 text-[9px] font-mono uppercase tracking-[0.08em] text-(--base-06) bg-(--base-02)">
                                System
                            </div>
                            <div className="py-1">
                                {items.map(n => (
                                    <div key={n.id} className="px-3 py-2.5 hover:bg-(--base-03) transition-colors">
                                        <div className="flex items-start gap-2">
                                            <AlertTriangle
                                                size={14}
                                                className={`mt-0.5 shrink-0 ${
                                                    n.severity === 'error' ? 'text-(--error-light)' : 'text-(--warning-light)'
                                                }`}
                                            />
                                            <div className="min-w-0 flex-1">
                                                <div className="text-sm font-medium text-(--base-09)">{n.title}</div>
                                                <div className="text-xs text-(--base-06) mt-0.5 leading-snug">{n.message}</div>
                                                {n.href && (
                                                    <Link
                                                        href={n.href}
                                                        onClick={() => setOpen(false)}
                                                        className="inline-flex items-center gap-1 mt-2 text-xs text-(--accent-light) hover:underline"
                                                    >
                                                        {n.cta || 'Go to fix'}
                                                        <ExternalLink size={11} />
                                                    </Link>
                                                )}
                                            </div>
                                        </div>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {/* User inbox — everyone */}
                    {hasInbox ? (
                        <div className="max-h-96 overflow-y-auto py-1">
                            {inbox.map(n => {
                                const unreadRow = !n.readAt;
                                const content = (
                                    <div className={`flex items-start gap-2 px-3 py-2.5 hover:bg-(--base-03) transition-colors ${unreadRow ? 'bg-(--accent-ghost)/30' : ''}`}>
                                        <Inbox size={14} className={`mt-0.5 shrink-0 ${unreadRow ? 'text-(--accent-light)' : 'text-(--base-06)'}`} />
                                        <div className="min-w-0 flex-1">
                                            <div className={`text-sm ${unreadRow ? 'font-medium text-(--base-09)' : 'text-(--base-07)'}`}>{n.title}</div>
                                            {n.body && (
                                                <div className="text-xs text-(--base-06) mt-0.5 leading-snug line-clamp-2">{n.body}</div>
                                            )}
                                            <div className="text-[10px] font-mono text-(--base-06) mt-1">{timeAgo(n.createdAt)}</div>
                                        </div>
                                    </div>
                                );
                                return n.link ? (
                                    <Link key={n.id} href={n.link} onClick={() => handleClickItem(n)}>
                                        {content}
                                    </Link>
                                ) : (
                                    <button key={n.id} type="button" onClick={() => handleClickItem(n)} className="w-full text-left">
                                        {content}
                                    </button>
                                );
                            })}
                        </div>
                    ) : (!isAdmin || !hasChecks) ? (
                        <div className="px-4 py-6 text-sm text-(--base-06) text-center">
                            All clear — nothing new.
                        </div>
                    ) : null}
                </div>
            )}
        </div>
    );
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
