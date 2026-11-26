"use client";

import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { Search, RefreshCw, UserCheck, X, Copy, Check, Network, Box, Server, AlertTriangle } from 'lucide-react';
import Segmented from '@/components/ui/Segmented';
import Tooltip from '@/components/ui/Tooltip';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, RowHead } from '@/components/ui/RowList';
import { getAdminServers, getUsers, AdminServer, User } from '@/lib/api';
import { StatusBadge } from '@/components/admin/StatusBadge';
import { AssignOwnerModal } from '@/components/admin/AssignOwnerModal';
import { SkeletonTable } from '@/components/Skeleton';
import { formatMB } from '@/lib/format';

// memory and diskLimit both arrive in MB.
function formatLimitMB(mb: number | undefined): string {
    return mb && mb > 0 ? formatMB(mb) : '-';
}

function formatRelativeDate(iso: string | undefined): string {
    if (!iso) return '-';
    const date = new Date(iso);
    const now = Date.now();
    const diffMs = now - date.getTime();
    const day = 24 * 60 * 60 * 1000;
    const diffDays = Math.floor(diffMs / day);
    if (diffDays < 1) return 'today';
    if (diffDays === 1) return '1d ago';
    if (diffDays < 30) return `${diffDays}d ago`;
    if (diffDays < 365) return `${Math.floor(diffDays / 30)}mo ago`;
    return `${Math.floor(diffDays / 365)}y ago`;
}

function CopyableUuid({ uuid }: { uuid: string }) {
    const [copied, setCopied] = useState(false);
    const copy = (e: React.MouseEvent) => {
        e.stopPropagation();
        navigator.clipboard.writeText(uuid);
        setCopied(true);
        setTimeout(() => setCopied(false), 1200);
    };
    return (
        <Tooltip content={copied ? 'Copied' : `${uuid} (click to copy)`}>
            <button
                type="button"
                onClick={copy}
                className="srv-num inline-flex items-center gap-1 rounded text-[11px] text-(--base-06) transition-colors hover:text-(--base-09) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                aria-label={`Copy UUID ${uuid}`}
            >
                {uuid.split('-')[0]}…
                {copied ? <Check size={12} className="text-(--success-light)" /> : <Copy size={12} className="opacity-60" />}
            </button>
        </Tooltip>
    );
}

function TypeBadge({ type }: { type: 'game' | 'proxy' | undefined }) {
    const isProxy = type === 'proxy';
    return (
        <span className="inline-flex items-center gap-1.5 text-xs text-(--base-07)">
            {isProxy ? <Network size={14} aria-hidden="true" /> : <Box size={14} aria-hidden="true" />}
            {isProxy ? 'Proxy' : 'Game'}
        </span>
    );
}

export default function AdminServersPage() {
    const [servers, setServers] = useState<AdminServer[]>([]);
    const [users, setUsers] = useState<User[]>([]);
    const [search, setSearch] = useState('');
    const [loading, setLoading] = useState(true);
    const [loadFailed, setLoadFailed] = useState(false);
    const [assignTarget, setAssignTarget] = useState<AdminServer | null>(null);
    const [refreshKey, setRefreshKey] = useState(0);
    // Which machines a server sits on. A segmented filter rather than a second
    // tab bar: this page already sits under the admin tab bar, and the panel
    // caps sub-navigation at one level.
    const [kindFilter, setKindFilter] = useState<'all' | 'platform' | 'external'>('all');

    const refresh = useCallback(() => setRefreshKey(k => k + 1), []);

    useEffect(() => {
        setLoading(true);
        Promise.all([getAdminServers(), getUsers()]).then(([sRes, uRes]) => {
            setServers(sRes.success ? (sRes.servers ?? []) : []);
            setUsers(uRes.success ? (uRes.users ?? []) : []);
            setLoadFailed(!sRes.success);
            setLoading(false);
        }).catch(() => { setLoadFailed(true); setLoading(false); });
    }, [refreshKey]);

    const filteredServers = useMemo(() => {
        let list = servers;
        if (search) {
            const q = search.toLowerCase();
            list = list.filter(s =>
                s.name.toLowerCase().includes(q) ||
                s.uuid.toLowerCase().includes(q) ||
                (s.owner ?? '').toLowerCase().includes(q)
            );
        }
        if (kindFilter !== 'all') {
            // A server on a node that has not reconnected since the external
            // tag was introduced reports as 'platform'. Treating a missing
            // value as platform keeps it visible somewhere rather than in
            // neither list.
            list = list.filter(s => (s.nodeKind ?? 'platform') === kindFilter);
        }
        return list;
    }, [servers, search, kindFilter]);

    const externalCount = useMemo(
        () => servers.filter(s => s.nodeKind === 'external').length,
        [servers]
    );

    return (
        <div className="flex flex-col gap-4">
            <div className="flex flex-wrap items-center gap-3">
                <div className="relative flex-1 min-w-48 max-w-sm">
                    <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)" aria-hidden="true" />
                    <input
                        type="text"
                        placeholder="Search by name, UUID or owner..."
                        aria-label="Search servers"
                        value={search}
                        onChange={e => setSearch(e.target.value)}
                        className="input-field input-icon-left input-icon-right w-full"
                    />
                    {search && (
                        <button
                            type="button"
                            onClick={() => setSearch('')}
                            aria-label="Clear search"
                            className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 text-(--base-06) hover:text-(--base-09) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                        >
                            <X size={14} />
                        </button>
                    )}
                </div>
                {/* Hidden until there is something to separate: on an install
                    with no external nodes this filter would only ever have one
                    populated option. */}
                {externalCount > 0 && (
                    <Segmented
                        ariaLabel="Filter by node kind"
                        value={kindFilter}
                        onChange={setKindFilter}
                        options={[
                            { id: 'all', label: 'All' },
                            { id: 'platform', label: 'Cluster' },
                            { id: 'external', label: 'External' },
                        ]}
                    />
                )}
                <span className="srv-num ml-auto text-xs text-(--base-06)">{filteredServers.length} server{filteredServers.length !== 1 ? 's' : ''}</span>
                <button onClick={refresh} className="btn btn-secondary btn-sm">
                    <RefreshCw size={14} />
                    Refresh
                </button>
            </div>

            {loading ? (
                <SkeletonTable rows={8} cols={5} />
            ) : loadFailed ? (
                <EmptyState
                    tone="error"
                    icon={<AlertTriangle size={20} />}
                    title="Could not load servers"
                    description="The server list did not load. Try again in a moment."
                    action={<button type="button" onClick={refresh} className="btn btn-secondary btn-sm"><RefreshCw size={14} /> Retry</button>}
                />
            ) : filteredServers.length === 0 ? (
                <EmptyState icon={<Server size={20} />} title="No servers found" description={search ? 'Nothing matches that search.' : undefined} />
            ) : (
                <div className="srv-surface overflow-x-auto">
                    <div className="min-w-[44rem]">
                        <RowList label="All servers">
                            <RowHead>
                                <span className="flex-1">Server</span>
                                <span className="w-32">Owner</span>
                                <span className="hidden w-20 md:block">Type</span>
                                <span className="hidden w-32 lg:block">Node</span>
                                <span className="hidden w-20 xl:block">Created</span>
                                <span className="hidden w-16 md:block">Members</span>
                                <span className="hidden w-24 lg:block">Memory / disk</span>
                                <span className="w-24">Status</span>
                                <span className="w-20" />
                            </RowHead>
                            {filteredServers.map(s => (
                                <div key={s.id} role="listitem" className="app-row">
                                    <div className="min-w-0 flex-1">
                                        <p className="truncate text-sm font-medium text-(--base-09)">{s.name}</p>
                                        <div className="mt-0.5 flex items-center gap-2">
                                            <CopyableUuid uuid={s.uuid} />
                                            {s.activeSubServer && (
                                                <span className="srv-num text-[11px] text-(--base-06)">{s.activeSubServer}</span>
                                            )}
                                        </div>
                                    </div>
                                    <span className="w-32 truncate text-sm text-(--base-07)">
                                        {s.owner ?? <span className="text-(--base-06)">Unassigned</span>}
                                    </span>
                                    <span className="hidden w-20 md:block"><TypeBadge type={s.serverType} /></span>
                                    <span className="hidden w-32 items-center gap-1.5 truncate text-xs text-(--base-07) lg:flex">
                                        {s.node}
                                        {s.nodeKind === 'external' && (
                                            <Tooltip content="Runs on your own hardware outside the swarm: gateway and beam only">
                                                <span className="srv-chip h-5 px-2 text-[11px]">External</span>
                                            </Tooltip>
                                        )}
                                    </span>
                                    <Tooltip content={s.createdAt || 'Unknown'}>
                                        <span className="srv-num hidden w-20 text-xs text-(--base-06) xl:block">{formatRelativeDate(s.createdAt)}</span>
                                    </Tooltip>
                                    <span className="srv-num hidden w-16 text-sm text-(--base-07) md:block">{s.memberCount ?? 0}</span>
                                    <span className="srv-num hidden w-24 text-xs leading-tight text-(--base-07) lg:block">
                                        <span className="block">{formatLimitMB(s.memory)}</span>
                                        <span className="block text-(--base-06)">{formatLimitMB(s.diskLimit)}</span>
                                    </span>
                                    <span className="w-24"><StatusBadge status={s.status} /></span>
                                    <span className="flex w-20 justify-end">
                                        <button
                                            type="button"
                                            onClick={() => setAssignTarget(s)}
                                            className="btn btn-ghost btn-sm"
                                            aria-label={`Assign owner for ${s.name}`}
                                        >
                                            <UserCheck size={14} />
                                            Assign
                                        </button>
                                    </span>
                                </div>
                            ))}
                        </RowList>
                    </div>
                </div>
            )}

            {assignTarget && (
                <AssignOwnerModal
                    server={assignTarget}
                    users={users}
                    onClose={() => setAssignTarget(null)}
                    onAssigned={refresh}
                />
            )}
        </div>
    );
}
