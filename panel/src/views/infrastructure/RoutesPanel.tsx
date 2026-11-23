"use client";

import { toast as notifyToast } from '@/components/ui/Toast';
import Tooltip from '@/components/ui/Tooltip';

import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import {
    getGatewayRoutes, deleteGatewayRoute, triggerGatewaySync, suspendGatewayRoute, unsuspendGatewayRoute,
    getRouteSuffixes, bulkDeleteRoutesBySuffix, RouteSuffix,
    GatewayRoute, GatewayEdge,
} from '@/lib/api';
import {
    Trash2, RefreshCw, Ban, Undo2, AlertTriangle, X, Search, Copy, Check, Layers, Globe, MoreHorizontal,
} from 'lucide-react';
import Menu from '@/components/ui/Menu';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, RowHead } from '@/components/ui/RowList';
import Callout from '@/components/server/Callout';
import { SkeletonTable, SkeletonText } from '@/components/Skeleton';
import ModalPanel from '@/components/ui/ModalPanel';

interface Toast {
    id: number;
    message: string;
    type: 'success' | 'error';
}

interface RoutesPanelProps {
    // Onlines edges drive the DNS-targets card and never change while a user
    // edits a route, so the parent (InfrastructureView) owns edge polling and
    // hands the current list down each render.
    onlineEdges: GatewayEdge[];
}

export default function RoutesPanel({ onlineEdges }: RoutesPanelProps) {
    const [routes, setRoutes] = useState<GatewayRoute[]>([]);
    const [loading, setLoading] = useState(true);
    const [search, setSearch] = useState('');
    const [deleteModal, setDeleteModal] = useState<GatewayRoute | null>(null);
    const [deleting, setDeleting] = useState(false);
    const [suspendModal, setSuspendModal] = useState<GatewayRoute | null>(null);
    const [suspendReason, setSuspendReason] = useState('');
    const [suspending, setSuspending] = useState(false);
    const [unsuspending, setUnsuspending] = useState<string | null>(null);

    const [copied, setCopied] = useState<string | null>(null);
    // The list polls every 5s and keeps its last answer, so a failure only
    // matters while there is nothing to show.
    const [loadFailed, setLoadFailed] = useState(false);


    const [bulkOpen, setBulkOpen] = useState(false);
    const [bulkSuffixes, setBulkSuffixes] = useState<RouteSuffix[]>([]);
    const [bulkSelected, setBulkSelected] = useState<string>('');
    const [bulkBusy, setBulkBusy] = useState(false);
    const [bulkConfirmCountdown, setBulkConfirmCountdown] = useState(0);

    const showToast = useCallback((message: string, type: 'success' | 'error' = 'success') => {
        notifyToast(message, type !== 'error');
    }, []);

    const fetchRoutes = useCallback(async () => {
        try {
            const res = await getGatewayRoutes();
            if (Array.isArray(res)) setRoutes(res);
            else if (res?.routes) setRoutes(res.routes);
            else { setLoadFailed(true); return; }
            setLoadFailed(false);
        } catch { setLoadFailed(true); }
        finally { setLoading(false); }
    }, []);

    useEffect(() => {
        fetchRoutes();
        const interval = setInterval(fetchRoutes, 5000);
        return () => clearInterval(interval);
    }, [fetchRoutes]);

    const openBulkDelete = async () => {
        setBulkSelected('');
        setBulkConfirmCountdown(0);
        setBulkOpen(true);
        const res = await getRouteSuffixes();
        setBulkSuffixes(res.success ? (res.suffixes || []) : []);
    };

    useEffect(() => {
        if (!bulkOpen || !bulkSelected) {
            setBulkConfirmCountdown(0);
            return;
        }
        setBulkConfirmCountdown(5);
        const id = setInterval(() => {
            setBulkConfirmCountdown(c => (c <= 1 ? 0 : c - 1));
        }, 1000);
        return () => clearInterval(id);
    }, [bulkOpen, bulkSelected]);

    const handleBulkDelete = async () => {
        if (!bulkSelected || bulkConfirmCountdown > 0) return;
        setBulkBusy(true);
        const res = await bulkDeleteRoutesBySuffix(bulkSelected);
        setBulkBusy(false);
        if (res.success) {
            showToast(`Deleted ${res.deleted} route(s) under .${bulkSelected}${res.failed ? `, ${res.failed} failed` : ''}`, res.failed ? 'error' : 'success');
            setBulkOpen(false);
            fetchRoutes();
        } else {
            showToast(res.message || 'Bulk delete failed', 'error');
        }
    };

    const handleSync = async () => {
        const res = await triggerGatewaySync();
        if (res.success !== false) showToast('Gateway sync triggered');
        else showToast(res.error || 'Sync failed', 'error');
    };

    const handleDeleteRoute = async () => {
        if (!deleteModal) return;
        const target = deleteModal.domain;
        setDeleting(true);
        try {
            const res = await deleteGatewayRoute(target);
            if (res.success !== false) {
                setRoutes(prev => prev.filter(r => r.domain !== target));
                showToast('Route deleted');
                setDeleteModal(null);
            } else {
                showToast(res.error || 'Failed to delete route', 'error');
            }
        } finally {
            setDeleting(false);
        }
    };

    const handleSuspend = async () => {
        if (!suspendModal || !suspendReason.trim()) return;
        setSuspending(true);
        try {
            const res = await suspendGatewayRoute(suspendModal.domain, suspendReason.trim());
            if (res.success !== false) {
                showToast('Route suspended');
                setSuspendModal(null);
                fetchRoutes();
            } else {
                showToast(res.message ?? res.error ?? 'Failed to suspend route', 'error');
            }
        } finally {
            setSuspending(false);
        }
    };

    const handleUnsuspend = async (route: GatewayRoute) => {
        if (unsuspending) return;
        setUnsuspending(route.domain);
        try {
            const res = await unsuspendGatewayRoute(route.domain);
            if (res.success !== false) {
                showToast('Route unsuspended');
                fetchRoutes();
            } else {
                showToast(res.message ?? res.error ?? 'Failed to unsuspend route', 'error');
            }
        } finally {
            setUnsuspending(null);
        }
    };

    const copyToClipboard = async (text: string) => {
        try {
            await navigator.clipboard.writeText(text);
            setCopied(text);
            setTimeout(() => setCopied(null), 1500);
        } catch {
            showToast('Copy failed', 'error');
        }
    };

    const filteredRoutes = useMemo(() => {
        if (!search.trim()) return routes;
        const q = search.toLowerCase();
        return routes.filter(r =>
            r.domain.toLowerCase().includes(q) ||
            (r.target_ip || '').toLowerCase().includes(q) ||
            (r.link_name || '').toLowerCase().includes(q) ||
            (r.server_name || '').toLowerCase().includes(q) ||
            (r.owner_name || '').toLowerCase().includes(q),
        );
    }, [routes, search]);

    if (loading) {
        return (
            <div className="flex flex-col gap-4" aria-busy="true">
                <div className="srv-surface p-5 flex flex-col gap-3">
                    <SkeletonText width="w-32" className="h-4" />
                    <SkeletonTable rows={6} cols={5} />
                </div>
            </div>
        );
    }

    return (
        <div className="flex flex-col gap-6">
            {loadFailed && routes.length === 0 && (
                <EmptyState
                    tone="error"
                    icon={<AlertTriangle size={20} />}
                    title="Could not load routes"
                    description="The gateway route list did not load. Check that the gateway is reachable, then try again."
                    action={<button type="button" onClick={fetchRoutes} className="btn btn-secondary btn-sm"><RefreshCw size={14} /> Retry</button>}
                />
            )}

            {/* DNS Targets */}
            {onlineEdges.length > 0 && (
                <Section title="DNS targets" description="Point your domains to one or more of these edge IPs.">
                    <div className="flex flex-wrap gap-2">
                        {onlineEdges.map(edge => (
                            <Tooltip key={edge.edge_id} content={`${edge.name}: click to copy`}>
                                <button
                                    type="button"
                                    onClick={() => copyToClipboard(edge.ip)}
                                    className="srv-chip h-8 gap-2 cursor-pointer"
                                >
                                    <span className="size-1.5 rounded-full bg-(--success-light)" aria-hidden="true" />
                                    <span className="srv-num text-(--base-09)">{edge.ip}</span>
                                    <span className="text-(--base-06)">{edge.name}</span>
                                    {copied === edge.ip
                                        ? <Check size={14} className="text-(--success-light)" />
                                        : <Copy size={14} className="text-(--base-06)" />
                                    }
                                </button>
                            </Tooltip>
                        ))}
                    </div>
                </Section>
            )}

            {/* Routes */}
            {!(loadFailed && routes.length === 0) && (
            <Section
                title="Routes"
                description={`${filteredRoutes.length}${search ? ` of ${routes.length}` : ''} ${routes.length === 1 ? 'route' : 'routes'} served by the gateway.`}
                actions={
                    <>
                        {routes.length > 0 && (
                            <Tooltip content="Delete all routes that share a domain suffix">
                                <button onClick={openBulkDelete} className="btn btn-secondary btn-sm">
                                    <Layers size={14} />
                                    Bulk delete
                                </button>
                            </Tooltip>
                        )}
                        <Tooltip content="Re-sync routes with the gateway">
                            <button onClick={handleSync} className="btn btn-secondary btn-sm">
                                <RefreshCw size={14} />
                                Sync
                            </button>
                        </Tooltip>
                    </>
                }
            >
                <div className="relative max-w-sm">
                    <Search size={14} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-06)" aria-hidden="true" />
                    <input
                        type="text"
                        placeholder="Search domain, target, link, server, owner..."
                        aria-label="Search routes"
                        value={search}
                        onChange={e => setSearch(e.target.value)}
                        className="input-field input-icon-left w-full"
                    />
                    {search && (
                        <button
                            onClick={() => setSearch('')}
                            aria-label="Clear search"
                            className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 text-(--base-06) hover:text-(--base-09) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                        >
                            <X size={14} />
                        </button>
                    )}
                </div>

                {routes.length === 0 ? (
                    <EmptyState icon={<Globe size={20} />} title="No routes configured" description="Add a route from a server's setup tab." />
                ) : filteredRoutes.length === 0 ? (
                    <EmptyState icon={<Search size={20} />} title={`No routes match "${search}"`} />
                ) : (
                    <div className="srv-surface overflow-x-auto">
                        <div className="min-w-[40rem]">
                            <RowList label="Routes">
                                <RowHead>
                                    <span className="flex-1">Domain and target</span>
                                    <span className="hidden w-32 md:block">Link</span>
                                    <span className="hidden w-32 lg:block">Server</span>
                                    <span className="hidden w-28 lg:block">Owner</span>
                                    <span className="w-8" />
                                </RowHead>
                                {filteredRoutes.map(route => (
                                    <div key={route.domain} role="listitem" className="app-row">
                                        <div className="min-w-0 flex-1">
                                            <div className="flex items-center gap-2">
                                                <span className="truncate text-sm font-medium text-(--base-09)">{route.domain}</span>
                                                {route.suspended && (
                                                    <Tooltip content={route.suspendedReason || 'Suspended'}>
                                                        <span className="srv-chip bg-(--error-ghost)! text-(--error-light)!">Suspended</span>
                                                    </Tooltip>
                                                )}
                                                {copied === route.domain && <Check size={14} className="text-(--success-light)" aria-label="Copied" />}
                                            </div>
                                            <div className="srv-num mt-0.5 truncate text-xs text-(--base-06)">
                                                {route.target_ip}:{route.target_port}
                                                {route.suspended && route.suspendedReason && (
                                                    <span className="ml-2 font-body text-(--base-06)">{route.suspendedReason}</span>
                                                )}
                                            </div>
                                        </div>
                                        <span className="hidden w-32 truncate text-sm text-(--base-07) md:block">{route.link_name || '-'}</span>
                                        <span className="hidden w-32 truncate text-sm text-(--base-07) lg:block">{route.server_name || '-'}</span>
                                        <span className="hidden w-28 truncate text-sm text-(--base-07) lg:block">{route.owner_name || '-'}</span>
                                        <Menu
                                            trigger={
                                                <button type="button" aria-label={`Actions for ${route.domain}`} className="app-icon-btn size-8!">
                                                    <MoreHorizontal size={16} />
                                                </button>
                                            }
                                            items={[
                                                { label: 'Copy domain', icon: <Copy size={14} />, onSelect: () => copyToClipboard(route.domain) },
                                                ...(route.core_owned ? [route.suspended
                                                    ? { label: 'Unsuspend route', icon: <Undo2 size={14} />, onSelect: () => handleUnsuspend(route), disabled: unsuspending !== null }
                                                    : { label: 'Suspend route', icon: <Ban size={14} />, onSelect: () => { setSuspendReason(''); setSuspendModal(route); } }] : []),
                                                { type: 'separator' as const },
                                                { label: 'Delete route', icon: <Trash2 size={14} />, danger: true, onSelect: () => setDeleteModal(route) },
                                            ]}
                                        />
                                    </div>
                                ))}
                            </RowList>
                        </div>
                    </div>
                )}
            </Section>
            )}

            {/* Suspend modal */}
            {suspendModal && (
                <div className="modal-overlay">
                    <ModalPanel className="modal-panel w-full max-w-sm">
                        <div className="modal-header flex items-center justify-between">
                            <h2 className="modal-title">Suspend route</h2>
                            <button onClick={() => setSuspendModal(null)} className="app-icon-btn size-8!" aria-label="Close">
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07)">
                                <span className="text-(--base-09) font-medium">{suspendModal.domain}</span> goes offline at once and its owner cannot re-create it. The reason is stored with the route.
                            </p>
                            <textarea
                                value={suspendReason}
                                onChange={e => setSuspendReason(e.target.value)}
                                maxLength={500}
                                rows={3}
                                placeholder="Reason (required)"
                                aria-label="Reason"
                                className="input-field w-full"
                            />
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setSuspendModal(null)} className="btn btn-secondary">
                                Cancel
                            </button>
                            <button
                                onClick={handleSuspend}
                                disabled={suspending || !suspendReason.trim()}
                                className="btn btn-danger"
                            >
                                {suspending ? 'Suspending...' : 'Suspend'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Delete confirm modal */}
            {deleteModal && (
                <div className="modal-overlay">
                    <ModalPanel className="modal-panel w-full max-w-sm">
                        <div className="modal-header flex items-center justify-between">
                            <h2 className="modal-title flex items-center gap-2">
                                <AlertTriangle size={16} className="text-(--error-light)" />
                                Delete route
                            </h2>
                            <button onClick={() => setDeleteModal(null)} className="app-icon-btn size-8!" aria-label="Close">
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Delete route <span className="text-(--base-09) font-medium">{deleteModal.domain}</span>?
                                This will immediately stop routing traffic through this entry.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeleteModal(null)} className="btn btn-secondary">
                                Cancel
                            </button>
                            <button onClick={handleDeleteRoute} disabled={deleting} className="btn btn-danger">
                                {deleting ? 'Deleting...' : 'Delete'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Bulk-delete-by-suffix modal */}
            {bulkOpen && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-full max-w-lg">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <Layers size={18} />
                                Bulk delete routes
                            </h3>
                            <button onClick={() => setBulkOpen(false)} className="app-icon-btn size-8!" aria-label="Close">
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07)">
                                Pick a domain suffix. Every route whose domain equals or ends with{' '}
                                <code className="srv-num text-(--base-09) bg-(--base-03) px-1 py-0.5 rounded">.{`<suffix>`}</code>{' '}
                                will be deleted. Independent of configured hoster domains.
                            </p>
                            {bulkSuffixes.length === 0 ? (
                                <p className="py-6 text-center text-[13px] text-(--base-06)">No suffixes found.</p>
                            ) : (
                                <div className="flex flex-col gap-0.5 max-h-72 overflow-y-auto rounded-lg bg-(--base-02) p-1.5">
                                    {bulkSuffixes.map(s => {
                                        const selected = bulkSelected === s.suffix;
                                        return (
                                            <button
                                                key={`${s.depth}-${s.suffix}`}
                                                type="button"
                                                onClick={() => setBulkSelected(s.suffix)}
                                                aria-pressed={selected}
                                                className={`flex items-center gap-2 px-2 py-1.5 rounded-md text-left transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${
                                                    selected ? 'bg-(--error-ghost)' : 'hover:bg-(--base-03)'
                                                }`}
                                            >
                                                <span className="srv-chip h-5 px-2 text-[11px]">
                                                    {s.depth === 1 ? 'apex' : 'sub'}
                                                </span>
                                                <span className="srv-num flex-1 text-sm text-(--base-09) truncate">
                                                    *.{s.suffix}
                                                </span>
                                                <span className="srv-num text-xs text-(--base-06)">{s.count} route{s.count !== 1 ? 's' : ''}</span>
                                            </button>
                                        );
                                    })}
                                </div>
                            )}
                            {bulkSelected && (
                                <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">
                                    This will permanently delete every route ending in{' '}
                                    <code className="srv-num">.{bulkSelected}</code>. Action cannot be undone.
                                </Callout>
                            )}
                        </div>
                        <div className="modal-footer">
                            <button
                                type="button"
                                onClick={() => setBulkOpen(false)}
                                disabled={bulkBusy}
                                className="btn btn-secondary"
                            >
                                Cancel
                            </button>
                            <button
                                type="button"
                                onClick={handleBulkDelete}
                                disabled={!bulkSelected || bulkBusy || bulkConfirmCountdown > 0}
                                className="btn btn-danger"
                            >
                                {bulkBusy
                                    ? <><RefreshCw size={14} className="animate-spin" /> Deleting...</>
                                    : bulkConfirmCountdown > 0
                                        ? `Delete (${bulkConfirmCountdown}s)`
                                        : <><Trash2 size={14} /> Delete all matching</>}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </div>
    );
}
