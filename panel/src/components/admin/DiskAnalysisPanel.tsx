"use client";

import React, { useState, useEffect } from 'react';
import {
    Trash2, AlertTriangle, ChevronDown, ChevronRight, Loader2, Check, X, UserPlus,
} from 'lucide-react';
import {
    getAdminDiskAnalysis, deleteOrphanedFolder, deleteServer,
    DiskAnalysis, Node,
} from '@/lib/api';
import { AssignOrphanModal } from './AssignOrphanModal';
import { nodeLabel } from '@/lib/nodeLabel';
import ModalPanel from '@/components/ui/ModalPanel';
import Callout from '@/components/server/Callout';

// Single confirm modal handles both flows (orphan folder + DB stray).
// `target` describes what's being deleted; null means closed.
type DeleteTarget =
    | { kind: 'orphan'; uuid: string }
    | { kind: 'dbStray'; id: number; serverName: string };

interface DiskAnalysisPanelProps {
    node: Node;
    onOrphanDeleted: () => void;
    autoLoad?: boolean;
    onAutoLoadConsumed?: () => void;
}

export function DiskAnalysisPanel({
    node, onOrphanDeleted, autoLoad, onAutoLoadConsumed,
}: DiskAnalysisPanelProps) {
    const [data, setData] = useState<DiskAnalysis | null>(null);
    const [loading, setLoading] = useState(false);
    const [deleting, setDeleting] = useState<string | null>(null);
    const [deletingDbId, setDeletingDbId] = useState<number | null>(null);
    const [expanded, setExpanded] = useState(false);
    const [pendingDelete, setPendingDelete] = useState<DeleteTarget | null>(null);
    const [deleteError, setDeleteError] = useState<string>('');
    const [assigningOrphan, setAssigningOrphan] = useState<string | null>(null);
    const [loadError, setLoadError] = useState('');

    const load = async () => {
        setLoading(true);
        setLoadError('');
        const res = await getAdminDiskAnalysis(node.id).catch(() => null);
        setLoading(false);
        if (!res || !res.success) {
            setLoadError('The analysis could not be run. Check that the node is reachable.');
            return;
        }
        {
            setData({
                nodeOnline: res.nodeOnline,
                matched: res.matched ?? [],
                orphaned: res.orphaned ?? [],
                missing: res.missing ?? [],
            });
            setExpanded(true);
        }
    };

    useEffect(() => {
        if (autoLoad && !data && !loading) {
            load();
            onAutoLoadConsumed?.();
        }
    }, [autoLoad]); // eslint-disable-line react-hooks/exhaustive-deps

    const handleDelete = (orphanUUID: string) => {
        setDeleteError('');
        setPendingDelete({ kind: 'orphan', uuid: orphanUUID });
    };

    const handleDeleteDbLeiche = (m: { id: number; serverName: string }) => {
        setDeleteError('');
        setPendingDelete({ kind: 'dbStray', id: m.id, serverName: m.serverName });
    };

    const confirmDelete = async () => {
        if (!pendingDelete) return;
        setDeleteError('');
        if (pendingDelete.kind === 'orphan') {
            setDeleting(pendingDelete.uuid);
            const res = await deleteOrphanedFolder(node.id, pendingDelete.uuid);
            setDeleting(null);
            if (res.success) {
                const uuid = pendingDelete.uuid;
                setData(prev => prev ? { ...prev, orphaned: prev.orphaned.filter(o => o.uuid !== uuid) } : prev);
                onOrphanDeleted();
                setPendingDelete(null);
            } else {
                setDeleteError(res.message ?? 'Delete failed');
            }
        } else {
            setDeletingDbId(pendingDelete.id);
            const res = await deleteServer(pendingDelete.id);
            setDeletingDbId(null);
            if (res.success) {
                const id = pendingDelete.id;
                setData(prev => prev ? { ...prev, missing: prev.missing.filter(x => x.id !== id) } : prev);
                onOrphanDeleted();
                setPendingDelete(null);
            } else {
                setDeleteError(res.message ?? 'Delete failed');
            }
        }
    };

    const orphanCount = data?.orphaned.length ?? 0;

    return (
        <div className="srv-surface overflow-hidden">
            <button
                type="button"
                onClick={() => expanded ? setExpanded(false) : (data ? setExpanded(true) : load())}
                aria-expanded={expanded}
                className="flex w-full items-center justify-between gap-3 px-5 py-4 text-left transition-colors hover:bg-(--base-03) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
            >
                <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1">
                    <span className={`size-2 shrink-0 rounded-full ${node.status === 'online' ? 'bg-(--success-light)' : 'bg-(--error)'}`} aria-hidden="true" />
                    <span className="text-sm font-medium text-(--base-09)">{nodeLabel(node)}</span>
                    {orphanCount > 0 && (
                        <span className="srv-chip bg-(--warning-ghost)! text-(--warning-light)!">
                            {orphanCount} orphaned
                        </span>
                    )}
                    {data && data.matched.length > 0 && (
                        <span className="srv-num text-xs text-(--base-06)">{data.matched.length} matched, {data.missing.length} missing</span>
                    )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
                    {loading && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-label="Running analysis" />}
                    {!data && !loading && (
                        <span className="text-xs text-(--accent-light)">Run analysis</span>
                    )}
                    {data && (expanded ? <ChevronDown size={16} className="text-(--base-06)" /> : <ChevronRight size={16} className="text-(--base-06)" />)}
                </div>
            </button>

            {loadError && !data && (
                <div className="px-5 pb-4">
                    <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{loadError}</Callout>
                </div>
            )}

            {expanded && data && (
                <div className="space-y-5 border-t border-(--base-03) p-5">
                    {data.orphaned.length > 0 && (
                        <div>
                            <p className="srv-label mb-2 flex items-center gap-1.5 text-(--warning-light)!">
                                <AlertTriangle size={14} aria-hidden="true" />
                                Orphaned (disk only, no DB record)
                            </p>
                            <div className="flex flex-col gap-0.5">
                                {data.orphaned.map(o => (
                                    <div key={o.uuid} className="app-row">
                                        <span className="srv-num min-w-0 flex-1 truncate text-xs text-(--base-07)">{o.uuid}</span>
                                        <div className="flex shrink-0 items-center gap-1.5">
                                            <button
                                                type="button"
                                                onClick={() => setAssigningOrphan(o.uuid)}
                                                className="btn btn-ghost btn-sm"
                                            >
                                                <UserPlus size={14} />
                                                Assign
                                            </button>
                                            <button
                                                type="button"
                                                onClick={() => handleDelete(o.uuid)}
                                                disabled={deleting === o.uuid}
                                                className="btn btn-ghost btn-sm text-(--error-light)!"
                                            >
                                                {deleting === o.uuid ? <Loader2 size={14} className="animate-spin" /> : <Trash2 size={14} />}
                                                Delete
                                            </button>
                                        </div>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {data.nodeOnline === false && (
                        <Callout tone="warning" icon={<AlertTriangle size={16} />}>
                            Node is offline / no heartbeat - DB orphans cannot be reliably detected.
                        </Callout>
                    )}

                    {data.nodeOnline !== false && data.missing.length > 0 && (
                        <div>
                            <p className="srv-label mb-2">
                                Missing from disk (DB-Leichen)
                            </p>
                            <div className="flex flex-col gap-0.5">
                                {data.missing.map(m => (
                                    <div key={m.uuid} className="app-row">
                                        <div className="min-w-0 flex-1">
                                            <span className="text-sm text-(--base-08)">{m.serverName}</span>
                                            <span className="srv-num ml-2 text-[11px] text-(--base-06)">{m.uuid}</span>
                                        </div>
                                        <button
                                            type="button"
                                            onClick={() => handleDeleteDbLeiche(m)}
                                            disabled={deletingDbId === m.id}
                                            className="btn btn-ghost btn-sm shrink-0 text-(--error-light)!"
                                        >
                                            {deletingDbId === m.id ? <Loader2 size={14} className="animate-spin" /> : <Trash2 size={14} />}
                                            Remove from DB
                                        </button>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {data.nodeOnline !== false && data.orphaned.length === 0 && data.missing.length === 0 && (
                        <p className="flex items-center gap-1.5 text-[13px] text-(--success-light)">
                            <Check size={16} />
                            All {data.matched.length} server folders match DB records
                        </p>
                    )}
                </div>
            )}

            {assigningOrphan && (
                <AssignOrphanModal
                    nodeId={node.id}
                    uuid={assigningOrphan}
                    onClose={() => setAssigningOrphan(null)}
                    onAssigned={() => {
                        const uuid = assigningOrphan;
                        setAssigningOrphan(null);
                        setData(prev => prev ? { ...prev, orphaned: prev.orphaned.filter(o => o.uuid !== uuid) } : prev);
                        onOrphanDeleted();
                    }}
                />
            )}

            {pendingDelete && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel w-full max-w-md">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                {pendingDelete.kind === 'orphan' ? 'Delete orphaned folder' : 'Remove stale DB entry'}
                            </h3>
                            <button onClick={() => setPendingDelete(null)} className="app-icon-btn size-8!" aria-label="Close">
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-3">
                            {deleteError && (
                                <Callout tone="error" icon={<AlertTriangle size={16} />} role="alert">{deleteError}</Callout>
                            )}
                            {pendingDelete.kind === 'orphan' ? (
                                <>
                                    <p className="text-sm text-(--base-08)">
                                        Permanently delete this orphaned folder from{' '}
                                        <span className="font-mono text-(--base-09)">{node.name}</span>?
                                    </p>
                                    <div className="srv-num rounded-lg bg-(--base-02) px-3 py-2 text-xs text-(--base-07) break-all">
                                        {pendingDelete.uuid}
                                    </div>
                                    <p className="text-xs text-(--base-06)">
                                        The folder has no matching DB record. Its contents will be removed from disk and cannot be recovered.
                                    </p>
                                </>
                            ) : (
                                <>
                                    <p className="text-sm text-(--base-08)">
                                        Remove the database entry for{' '}
                                        <span className="font-mono text-(--base-09)">{pendingDelete.serverName}</span>?
                                    </p>
                                    <p className="text-xs text-(--base-06)">
                                        The on-disk folder no longer exists on this node, so the DB row is a leftover. Removing it cleans up dashboards and stats; nothing is touched on disk.
                                    </p>
                                </>
                            )}
                        </div>
                        <div className="modal-footer">
                            <button
                                type="button"
                                onClick={() => setPendingDelete(null)}
                                className="btn btn-secondary"
                                disabled={!!deleting || !!deletingDbId}
                            >
                                Cancel
                            </button>
                            <button
                                type="button"
                                onClick={confirmDelete}
                                disabled={!!deleting || !!deletingDbId}
                                className="btn btn-danger"
                            >
                                {deleting || deletingDbId ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                                {pendingDelete.kind === 'orphan' ? 'Delete folder' : 'Remove entry'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </div>
    );
}
