import { CircleDot, CircleCheckBig, Clock, AlertTriangle } from 'lucide-react';
import type { TicketStatus, TicketPriority } from '@/lib/api/tickets';
import '@/styles/tickets.css';

const STATUS: Record<TicketStatus, { icon: typeof CircleDot; tone: string; label: string }> = {
    open:         { icon: CircleDot,      tone: 'accent',  label: 'Open' },
    in_progress:  { icon: Clock,          tone: 'warning', label: 'In progress' },
    waiting_user: { icon: AlertTriangle,  tone: 'warning', label: 'Waiting' },
    resolved:     { icon: CircleCheckBig, tone: 'success', label: 'Resolved' },
    closed:       { icon: CircleCheckBig, tone: 'muted',   label: 'Closed' },
};

export function StatusBadge({ status }: { status: TicketStatus }) {
    const m = STATUS[status] || STATUS.open;
    const Icon = m.icon;
    return <span className="tk-pill" data-tone={m.tone}><Icon size={12} aria-hidden="true" />{m.label}</span>;
}

export function PriorityBadge({ priority }: { priority: TicketPriority }) {
    const tone = priority === 'urgent' ? 'error' : priority === 'high' ? 'warning' : priority === 'low' ? 'muted' : undefined;
    return <span className="tk-pill capitalize" data-tone={tone}>{priority}</span>;
}

export function timeAgo(iso: string): string {
    try {
        const diff = Date.now() - new Date(iso).getTime();
        const min = Math.floor(diff / 60000);
        if (min < 1) return 'just now';
        if (min < 60) return `${min}m ago`;
        const hr = Math.floor(min / 60);
        if (hr < 24) return `${hr}h ago`;
        const d = Math.floor(hr / 24);
        if (d < 30) return `${d}d ago`;
        return `${Math.floor(d / 30)}mo ago`;
    } catch { return ''; }
}
