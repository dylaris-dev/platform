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
import { keyActorLabel } from '@/lib/auditSentences';
import { SkeletonCard, SkeletonHeader } from '@/components/Skeleton';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import Callout from '@/components/server/Callout';
import { RowList } from '@/components/ui/RowList';
import Select from '@/components/ui/Select';
import { fmtDateTime } from '@/lib/time';

// The event types the trail actually produces, most-asked-about first. An
// unknown type still renders, verbatim - a filter list that silently hides a
// new event type is worse than one that is a little out of date.
const FILTERS: { id: string; label: string }[] = [
    { id: '', label: 'All events' },
    { id: 'user_hard_deleted', label: 'Deletions' },
    { id: 'user_anonymized', label: 'Anonymised' },
    { id: 'user_registered', label: 'Registrations' },
    { id: 'user_created', label: 'Accounts created by an operator' },
    { id: 'password_set_by_admin', label: 'Passwords set by an operator' },
    { id: 'billing_status_changed', label: 'Billing changes' },
    { id: 'server_owner_changed', label: 'Server owner changes' },
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
    user_created: 'Account created by an operator',
    password_set_by_admin: 'Password set by an operator',
    billing_status_changed: 'Billing status changed',
    server_owner_changed: 'Server given to another account',
    user_email_changed: 'Address changed',
    gateway_public_targets_changed: 'Public API route targets changed',
    gateway_route_suspended: 'Route suspended by an operator',
    gateway_route_unsuspended: 'Route suspension lifted by an operator',
    gateway_provider_changed: 'Gateway provider changed',
    managed_link_kit_viewed: 'Managed link deploy kit viewed',
    managed_link_kit_retired: "Node's managed link replaced",
    provider_orphan_kit_revoked: 'Unknown provider link revoked',
    user_role_changed: 'Role changed',
    user_permissions_changed: 'Permissions changed',
    '2fa_admin_reset': 'Two-factor reset by an operator',
    '2fa_setup_completed': 'Two-factor set up',
    '2fa_backup_code_consumed': 'Backup code used',
    sessions_revoked: 'Signed out everywhere',
    email_verified: 'Address verified',
    password_reset_requested: 'Password reset requested',
    account_grant_assigned: 'Account-wide access granted',
    account_grant_revoked: 'Account-wide access revoked',
    user_panel_role_changed: 'Staff role changed',
    panel_role_created: 'Staff role created',
    panel_role_updated: 'Staff role edited',
    panel_role_deleted: 'Staff role deleted',
    server_role_created: 'Server role created',
    server_role_updated: 'Server role edited',
    server_role_deleted: 'Server role deleted',
    'dns_account.created': 'DNS account added',
    'dns_account.updated': 'DNS account edited',
    'dns_account.deleted': 'DNS account deleted',
    'dns_settings.updated': 'DNS settings changed',
    maintenance_toggled: 'Maintenance mode changed',
    // Moving the platform's own data. All five of these used to arrive as
    // maintenance_toggled, so this page said "Maintenance mode changed" for
    // somebody moving the entire database to another host.
    db_migration_started: 'Database migration started',
    db_hypertable_converted: 'Statistics table converted',
    storage_migration_started: 'Storage migration started',
    storage_migration_cancelled: 'Storage migration cancelled',
    storage_manifest_deleted: 'Storage manifest deleted',
    storage_orphans_deleted: 'Orphaned storage files deleted',
    storage_lifecycle_applied: 'Storage lifecycle rules applied',
    api_key_created: 'API key created',
    api_key_revoked: 'API key revoked',
    api_key_rolled: 'API key rolled',
    api_keys_revoked: 'API keys revoked',
    api_key_denied: 'API key request refused',
    api_key_used: 'API key request',
    api_key_read: 'API key reads',
    api_key_rate_limited: 'API key rate limited',
    deletion_cancelled_by_api_key: 'Deletion cancelled: API key in use',
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
            <PageHeader
                as="h2"
                icon={<History size={18} />}
                title="Identity log"
                description="What has happened to accounts: registrations, address and role changes, two-factor resets and deletions. Append-only. A deleted account keeps its name and address here and nowhere else."
                actions={
                    <>
                        <div className="w-56">
                            <Select
                                value={filter}
                                onChange={setFilter}
                                options={FILTERS.map(f => ({ value: f.id, label: f.label }))}
                                ariaLabel="Filter by event type"
                            />
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
                    </>
                }
            />

            {/* A failed refresh keeps the rows already shown; only a first load
                with nothing to show becomes the error state. */}
            {error && rows.length > 0 && (
                <Callout tone="error" icon={<ShieldAlert size={16} />} title="The identity log could not be refreshed">{error}</Callout>
            )}
            {error && rows.length === 0 ? (
                <EmptyState tone="error" icon={<ShieldAlert size={20} />} title="The identity log could not be loaded" description={error} />
            ) : loading ? (
                <div className="srv-surface p-5 space-y-3">
                    <SkeletonHeader />
                    <SkeletonCard height="h-12" />
                    <SkeletonCard height="h-12" />
                    <SkeletonCard height="h-12" />
                </div>
            ) : rows.length === 0 ? (
                <EmptyState icon={<History size={20} />} title="Nothing recorded for this filter yet" />
            ) : (
                <div className="srv-surface">
                    <RowList label="Identity events">
                        {rows.map(ev => <EventRow key={ev.id} ev={ev} />)}
                    </RowList>
                </div>
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
    const hasMeta = !!ev.metadata && Object.keys(ev.metadata).length > 0;
    return (
        <div role="listitem" className="app-row flex-col !items-stretch">
            <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                    <p className="text-sm font-medium text-(--base-09)">
                        {label}
                        {subject && <> &middot; <span className="font-mono text-(--accent-light)">{subject}</span></>}
                    </p>
                    <p className="mt-0.5 font-mono text-xs text-(--base-06)">
                        {ev.actorApiKeyName
                            ? <>by <span className="text-(--base-08)">{keyActorLabel(ev.actorApiKeyName, ev.actorName || ev.actorUserId)}</span></>
                            : ev.actorName
                            ? <>by <span className="text-(--base-08)">{ev.actorName}</span></>
                            : ev.actorUserId ? <>by {ev.actorUserId}</> : 'by the system'}
                        {ev.ipAddress && <> &middot; {ev.ipAddress}</>}
                    </p>
                </div>
                <time className="shrink-0 font-mono text-xs text-(--base-06)" title={ev.createdAt}>
                    {fmtDateTime(ev.createdAt)}
                </time>
            </div>
            {hasMeta && (
                <pre className="mt-2 overflow-x-auto rounded-lg bg-(--base-01) p-2 font-mono text-[11px] text-(--base-07)">
{JSON.stringify(ev.metadata, null, 2)}
                </pre>
            )}
        </div>
    );
}
