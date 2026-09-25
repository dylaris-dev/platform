"use client";

// The identity audit trail: what has happened to ACCOUNTS.
//
// The rows have been written for a while and nothing read them - no route, no
// screen - so the only way to answer "what did I just remove" was a database
// shell. A deletion is the sharpest case: the account is gone, so its username
// and address survive only in the row's metadata, which is what this renders
// when there is no target left to name.

import React, { useEffect, useState } from 'react';
import { History, RefreshCw, Loader2, ShieldAlert } from 'lucide-react';

import { listIdentityAudit, type IdentityAuditEvent } from '@/lib/api/identityAudit';
import { SkeletonCard, SkeletonHeader } from '@/components/Skeleton';

// The event types the trail actually produces, most-asked-about first. An
// unknown type still renders, verbatim - a filter list that silently hides a
// new event type is worse than one that is a little out of date.
const FILTERS: { id: string; label: string }[] = [
    { id: '', label: 'All events' },
    { id: 'user_hard_deleted', label: 'Deletions' },
    { id: 'user_anonymized', label: 'Anonymised' },
    { id: 'user_registered', label: 'Registrations' },
    { id: 'user_email_changed', label: 'Address changes' },
    { id: 'user_role_changed', label: 'Role changes' },
    { id: 'user_permissions_changed', label: 'Permission changes' },
    { id: '2fa_admin_reset', label: 'Two-factor resets' },
    { id: 'storage_migration_started', label: 'Storage migrations' },
    { id: 'db_migration_started', label: 'Database migrations' },
];

const LABELS: Record<string, string> = {
    user_hard_deleted: 'Account deleted',
    user_anonymized: 'Account anonymised',
    user_registered: 'Account registered',
    user_email_changed: 'Address changed',
    user_role_changed: 'Role changed',
    user_permissions_changed: 'Permissions changed',
    '2fa_admin_reset': 'Two-factor reset by an operator',
    '2fa_setup_completed': 'Two-factor set up',
    '2fa_backup_code_consumed': 'Backup code used',
    email_verified: 'Address verified',
    password_reset_requested: 'Password reset requested',
    account_grant_assigned: 'Account-wide access granted',
    maintenance_toggled: 'Maintenance mode changed',
    // Moving the platform's own data. All five of these used to arrive as
    // maintenance_toggled, so this page said "Maintenance mode changed" for
    // somebody moving the entire database to another host.
    db_migration_started: 'Database migration started',
    db_hypertable_converted: 'Statistics table converted',
    storage_migration_started: 'Storage migration started',
    storage_migration_cancelled: 'Storage migration cancelled',
    storage_manifest_deleted: 'Storage manifest deleted',
};

export default function IdentityLogPage() {
    const [rows, setRows] = useState<IdentityAuditEvent[]>([]);
    const [filter, setFilter] = useState('');
    const [loading, setLoading] = useState(true);
    const [refreshing, setRefreshing] = useState(false);
    const [error, setError] = useState('');

    const load = async (reset = false) => {
        if (reset) setLoading(true); else setRefreshing(true);
        const res = await listIdentityAudit({ eventType: filter || undefined, limit: 200 });
        setLoading(false);
        setRefreshing(false);
        if (res.success) {
            setRows(res.events || []);
            setError('');
        } else {
            setError(res.message || 'The identity log could not be loaded.');
        }
    };

    useEffect(() => {
        load(true);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [filter]);

    return (
        <div className="space-y-6 max-w-5xl">
            <header className="flex items-start justify-between gap-4 flex-wrap">
                <div>
                    <h2 className="h-section flex items-center gap-2">
                        <History size={18} className="text-(--accent-light)" /> Identity log
                    </h2>
                    <p className="text-sm text-(--base-06) mt-1">
                        What has happened to accounts: registrations, address and role changes, two-factor
                        resets and deletions. Append-only. A deleted account keeps its name and address here
                        and nowhere else.
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

            <div className="flex flex-wrap gap-1.5">
                {FILTERS.map(f => (
                    <button
                        key={f.id}
                        type="button"
                        onClick={() => setFilter(f.id)}
                        className={`px-2.5 py-1 rounded text-xs transition-colors ${
                            filter === f.id
                                ? 'bg-(--accent-ghost) text-(--accent-light)'
                                : 'text-(--base-06) hover:text-(--base-08) hover:bg-(--base-03)'
                        }`}
                    >
                        {f.label}
                    </button>
                ))}
            </div>

            {error && (
                <div className="card p-4 border border-(--base-03) flex items-center gap-2">
                    <ShieldAlert size={14} className="text-(--error-light) shrink-0" />
                    <p className="text-sm text-(--error-light)">{error}</p>
                </div>
            )}

            {loading ? (
                <div className="card p-5 space-y-3 border border-(--base-03)">
                    <SkeletonHeader />
                    <SkeletonCard height="h-12" />
                    <SkeletonCard height="h-12" />
                    <SkeletonCard height="h-12" />
                </div>
            ) : rows.length === 0 ? (
                <div className="card p-10 border border-(--base-03) text-center text-(--base-06)">
                    <History size={24} className="mx-auto" />
                    <p className="mt-3 text-sm">Nothing recorded for this filter yet.</p>
                </div>
            ) : (
                <ul className="space-y-2">
                    {rows.map(ev => <EventRow key={ev.id} ev={ev} />)}
                </ul>
            )}
        </div>
    );
}

// who names the account an event is ABOUT. A deletion has no target id left to
// join on - the row is written after the account is gone - so the metadata is
// the only place its identity survives.
function who(ev: IdentityAuditEvent): string {
    if (ev.targetName) return ev.targetName;
    const meta = ev.metadata || {};
    const name = typeof meta.username === 'string' ? meta.username : '';
    const mail = typeof meta.email === 'string' ? meta.email : '';
    if (name && mail) return `${name} (${mail})`;
    if (name) return name;
    if (mail) return mail;
    if (ev.targetUserId) return ev.targetUserId;
    return '';
}

function EventRow({ ev }: { ev: IdentityAuditEvent }) {
    const label = LABELS[ev.eventType] || ev.eventType;
    const subject = who(ev);
    return (
        <li className="card p-3 border border-(--base-03)">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="text-sm font-medium text-(--base-09)">
                        {label}
                        {subject && <> · <span className="font-mono text-(--accent-light)">{subject}</span></>}
                    </p>
                    <p className="text-xs text-(--base-06) mt-0.5 font-mono">
                        {ev.actorName
                            ? <>by <span className="text-(--base-08)">{ev.actorName}</span></>
                            : ev.actorUserId ? <>by {ev.actorUserId}</> : 'by the system'}
                        {ev.ipAddress && <> · {ev.ipAddress}</>}
                    </p>
                </div>
                <time className="text-[10px] font-mono text-(--base-06) shrink-0" title={ev.createdAt}>
                    {new Date(ev.createdAt).toLocaleString()}
                </time>
            </div>
            {ev.metadata && Object.keys(ev.metadata).length > 0 && (
                <pre className="mt-2 text-[10px] font-mono text-(--base-07) bg-(--base-02) border border-(--base-03) rounded p-2 overflow-x-auto">
{JSON.stringify(ev.metadata, null, 2)}
                </pre>
            )}
        </li>
    );
}
