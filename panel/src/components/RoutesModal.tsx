"use client";

import React, { useState, useEffect, useCallback, useRef } from 'react';
import CopyText from '@/components/ui/CopyText';
import {
    getServerRoutes, createServerRoute, deleteServerRoute, checkDomainAvailability,
    GatewayRoute, CreateRouteRequest,
} from '@/lib/api';
import { Globe, Plus, Trash2, X, Check, AlertTriangle } from 'lucide-react';
import RouteDomainPicker from '@/components/RouteDomainPicker';
import ModalPanel from '@/components/ui/ModalPanel';

interface RoutesModalProps {
    serverId: number;
    serverName: string;
    onClose: () => void;
    onRoutesChanged?: (routes: GatewayRoute[]) => void;
}

type AvailabilityState =
    | { status: 'idle' }
    | { status: 'checking' }
    | { status: 'available'; domain: string }
    | { status: 'taken'; domain: string }
    | { status: 'invalid'; reason: string };

export default function RoutesModal({ serverId, serverName, onClose, onRoutesChanged }: RoutesModalProps) {
    const [routes, setRoutes] = useState<GatewayRoute[]>([]);
    const [routesLoading, setRoutesLoading] = useState(false);
    const [newRoute, setNewRoute] = useState<CreateRouteRequest>({ targetPort: 25565 });
    const [routeError, setRouteError] = useState('');
    // A route on an unproven custom domain is accepted but provisional: a
    // four-hour clock starts and missing it deletes the route. Core returns the
    // instruction; without showing it the customer's first sign is the route
    // being gone.
    const [routeNotice, setRouteNotice] = useState('');
    const [routeCreating, setRouteCreating] = useState(false);
    const [deleteTarget, setDeleteTarget] = useState<GatewayRoute | null>(null);
    const [deleteError, setDeleteError] = useState('');
    const [deleting, setDeleting] = useState(false);
    const [availability, setAvailability] = useState<AvailabilityState>({ status: 'idle' });

    // The callback is held in a ref, NOT in loadRoutes' dependency list. A parent
    // that passes an inline arrow (SetupView did) hands a new function identity on
    // every render; with it as a dependency, loadRoutes was new every render too,
    // the mount effect re-ran, its result called back into the parent, the parent
    // re-rendered - an endless refetch that read on screen as a modal refreshing
    // forever. The ref keeps the latest callback reachable without making the
    // loader's identity depend on it.
    const onRoutesChangedRef = useRef(onRoutesChanged);
    onRoutesChangedRef.current = onRoutesChanged;

    const loadRoutes = useCallback(async () => {
        setRoutesLoading(true);
        try {
            const res = await getServerRoutes(serverId);
            const next: GatewayRoute[] = Array.isArray(res) ? res : (res.routes ?? []);
            setRoutes(next);
            onRoutesChangedRef.current?.(next);
        } catch { setRoutes([]); }
        setRoutesLoading(false);
    }, [serverId]);

    useEffect(() => { loadRoutes(); }, [loadRoutes]);

    const hasRouteInput = !!(newRoute.subdomain || newRoute.customDomain || newRoute.domain);

    useEffect(() => {
        if (!hasRouteInput) {
            setAvailability({ status: 'idle' });
            return;
        }
        setAvailability({ status: 'checking' });
        const handle = setTimeout(async () => {
            try {
                const res = await checkDomainAvailability({
                    domain: newRoute.domain,
                    subdomain: newRoute.subdomain,
                    hosterDomain: newRoute.hosterDomain,
                    customDomain: newRoute.customDomain,
                });
                if (res.reason) {
                    setAvailability({ status: 'invalid', reason: res.reason });
                } else if (res.available) {
                    setAvailability({ status: 'available', domain: res.domain || '' });
                } else {
                    setAvailability({ status: 'taken', domain: res.domain || '' });
                }
            } catch {
                setAvailability({ status: 'idle' });
            }
        }, 350);
        return () => clearTimeout(handle);
    }, [hasRouteInput, newRoute.domain, newRoute.subdomain, newRoute.hosterDomain, newRoute.customDomain]);

    const handleCreateRoute = async () => {
        setRouteError('');
        setRouteNotice('');
        if (!hasRouteInput) { setRouteError('Domain is required'); return; }
        setRouteCreating(true);
        try {
            const res = await createServerRoute(serverId, newRoute);
            if (res.error) {
                setRouteError(res.error);
                setRouteCreating(false);
                return;
            }
            setRouteNotice((res as { ownershipNotice?: string }).ownershipNotice || '');
            const resolvedDomain: string | undefined = res.domain || newRoute.domain || newRoute.customDomain
                || (newRoute.subdomain && newRoute.hosterDomain ? `${newRoute.subdomain}.${newRoute.hosterDomain}` : undefined);
            if (resolvedDomain) {
                setRoutes(prev => prev.some(r => r.domain === resolvedDomain)
                    ? prev
                    : [...prev, { domain: resolvedDomain, target_ip: '', target_port: newRoute.targetPort }]);
            }
            setNewRoute({ targetPort: 25565 });
            setAvailability({ status: 'idle' });
            // Poll for Hub-committed route
            for (let i = 0; i < 8; i++) {
                await new Promise(r => setTimeout(r, i === 0 ? 400 : 800));
                try {
                    const list = await getServerRoutes(serverId);
                    const real: GatewayRoute[] = Array.isArray(list) ? list : (list.routes ?? []);
                    if (!resolvedDomain || real.some(r => r.domain === resolvedDomain)) {
                        setRoutes(real);
                        onRoutesChangedRef.current?.(real);
                        break;
                    }
                } catch { /* keep polling */ }
            }
        } catch { setRouteError('Failed to create route'); }
        setRouteCreating(false);
    };

    const handleConfirmDelete = async () => {
        if (!deleteTarget) return;
        const target = deleteTarget.domain;
        setDeleting(true);
        setDeleteError('');
        try {
            // fetchAPI does not throw on 4xx/5xx - it RESOLVES with
            // {success:false}. So this try/catch only ever caught a dead network,
            // and every refusal Core has here fell straight through to the filter
            // below: the route vanished from the list, the dialog closed, and the
            // route was still routing traffic. The user is told an entry is gone
            // that is not. Read the answer instead, and only then drop the row.
            const res = await deleteServerRoute(serverId, target);
            if (res?.success === false) {
                setDeleteError(res.message || res.error || 'The route could not be deleted.');
                setDeleting(false);
                return;
            }
            const next = routes.filter(r => r.domain !== target);
            setRoutes(next);
            onRoutesChangedRef.current?.(next);
        } catch {
            setDeleteError('The request could not be sent.');
            setDeleting(false);
            return;
        }
        setDeleting(false);
        setDeleteTarget(null);
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-2xl flex flex-col max-h-[85vh]" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <div>
                        <h3 className="modal-title flex items-center gap-2">
                            <Globe size={16} className="text-(--accent-light)" />
                            Routes &amp; Domains
                        </h3>
                        <p className="text-xs text-(--base-07) mt-1">{serverName}</p>
                    </div>
                    <button onClick={onClose} className="p-1 text-(--base-06) hover:text-(--base-09) transition-colors">
                        <X size={18} />
                    </button>
                </div>

                <div className="modal-body flex-1 overflow-y-auto space-y-4">
                    <p className="text-sm text-(--base-07)">Map custom domains to this server through the gateway. Players connect to these domains instead of the raw IP.</p>

                    {routesLoading ? (
                        <p className="text-sm text-(--base-07)">Loading routes…</p>
                    ) : routes.length > 0 ? (
                        <div className="space-y-2">
                            {routes.map(route => (
                                <div key={route.domain} className="flex items-center justify-between bg-(--base-02) rounded-md px-3 py-2 border border-(--base-03)">
                                    <div className="flex items-center gap-2.5">
                                        <Globe size={12} className="text-(--accent-light) shrink-0" />
                                        <div>
                                            <CopyText
                                                value={route.domain}
                                                className="text-sm font-medium text-(--base-09) max-w-full"
                                            />
                                            <div className="mono-label">
                                                Port {route.target_port}
                                                {route.link_name && <> &middot; Link: {route.link_name}</>}
                                            </div>
                                        </div>
                                    </div>
                                    <button
                                        onClick={() => setDeleteTarget(route)}
                                        className="text-(--error-light) hover:text-(--error) transition-colors p-1"
                                        title="Delete route"
                                    >
                                        <Trash2 size={14} />
                                    </button>
                                </div>
                            ))}
                        </div>
                    ) : (
                        <p className="alert alert-info text-xs">No routes configured yet. Add your first one below.</p>
                    )}

                    <div className="bg-(--base-03) rounded-md border border-(--base-04) p-3 space-y-2">
                        <div className="mono-label">Add Route</div>
                        <RouteDomainPicker
                            value={newRoute}
                            onChange={next => { setNewRoute(next); setRouteError(''); setRouteNotice(''); }}
                            error={routeError}
                            portChildren={
                                <div className="flex gap-2">
                                    <select
                                        value={newRoute.targetPort}
                                        onChange={e => setNewRoute(r => ({ ...r, targetPort: Number(e.target.value) }))}
                                        className="input-field text-sm w-32"
                                    >
                                        <option value={25565}>MC (25565)</option>
                                    </select>
                                    <button
                                        onClick={handleCreateRoute}
                                        disabled={routeCreating || !hasRouteInput || availability.status === 'taken' || availability.status === 'invalid'}
                                        className="btn btn-primary btn-sm disabled:opacity-40"
                                    >
                                        <Plus size={13} />
                                        Add
                                    </button>
                                </div>
                            }
                        />

                        {routeNotice && (
                            <p className="alert alert-warning text-xs">{routeNotice}</p>
                        )}

                        {hasRouteInput && (
                            <div className="text-[11px] font-mono min-h-3.5">
                                {availability.status === 'checking' && (
                                    <span className="text-(--base-06)">Checking…</span>
                                )}
                                {availability.status === 'available' && (
                                    <span className="text-(--success-light) inline-flex items-center gap-1">
                                        <Check size={11} />
                                        {availability.domain || 'Domain'} is available
                                    </span>
                                )}
                                {availability.status === 'taken' && (
                                    <span className="text-(--error-light) inline-flex items-center gap-1">
                                        <X size={11} />
                                        {availability.domain || 'Domain'} is already in use
                                    </span>
                                )}
                                {availability.status === 'invalid' && (
                                    <span className="text-(--warning-light) inline-flex items-center gap-1">
                                        <AlertTriangle size={11} />
                                        {availability.reason}
                                    </span>
                                )}
                            </div>
                        )}
                    </div>
                </div>

                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Close</button>
                </div>

                {deleteTarget && (
                    <div className="modal-overlay animate-fade-in" onClick={() => setDeleteTarget(null)}>
                        <ModalPanel onClose={() => setDeleteTarget(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                            <div className="modal-header">
                                <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                    <AlertTriangle size={16} /> Delete Route
                                </h3>
                            </div>
                            <div className="modal-body">
                                <p className="text-sm text-(--base-07)">
                                    Delete route <span className="text-(--base-09) font-medium">{deleteTarget.domain}</span>?
                                    This will immediately stop routing traffic through this entry.
                                </p>
                                {deleteError && (
                                    <div className="alert alert-error text-xs mt-3" role="alert">{deleteError}</div>
                                )}
                            </div>
                            <div className="modal-footer">
                                <button onClick={() => { setDeleteTarget(null); setDeleteError(''); }} disabled={deleting} className="btn btn-secondary">Cancel</button>
                                <button onClick={handleConfirmDelete} disabled={deleting} className="btn btn-danger">
                                    {deleting ? 'Deleting…' : 'Delete'}
                                </button>
                            </div>
                        </ModalPanel>
                    </div>
                )}
            </ModalPanel>
        </div>
    );
}
