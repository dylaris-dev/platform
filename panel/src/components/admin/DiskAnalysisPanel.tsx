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

    const load = async () => {
        setLoading(true);
        const res = await getAdminDiskAnalysis(node.id);
        setLoading(false);
        if (res.success) {
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
        <div className="border border-(--base-03) rounded-lg overflow-hidden">
            <button
                onClick={() => expanded ? setExpanded(false) : (data ? setExpanded(true) : load())}
                className="w-full flex items-center justify-between px-4 py-3 bg-(--base-02) hover:bg-(--base-03) transition-colors text-left"
            >
                <div className="flex items-center gap-2.5">
                    <div className={`w-1.5 h-1.5 rounded-full ${node.status === 'online' ? 'bg-(--success-light)' : 'bg-(--error)'}`} />
                    <span className="text-sm font-medium text-(--base-09)">{nodeLabel(node)}</span>
                    {orphanCount > 0 && (
                        <span className="badge bg-(--warning-ghost) text-(--warning) border border-(--warning-border) text-[10px]">
                            {orphanCount} orphaned
                        </span>
                    )}
                    {data && data.matched.length > 0 && (
                        <span className="text-[11px] text-(--base-06)">{data.matched.length} matched · {data.missing.length} missing</span>
                    )}
                </div>
                <div className="flex items-center gap-2">
                    {loading && <Loader2 size={14} className="animate-spin text-(--base-05)" />}
                    {!data && !loading && (
                        <span className="text-[11px] text-(--accent-light)">Run analysis</span>
                    )}
                    {data && (expanded ? <ChevronDown size={14} className="text-(--base-05)" /> : <ChevronRight size={14} className="text-(--base-05)" />)}
                </div>
            </button>

            {expanded && data && (
                <div className="p-4 space-y-4">
                    {data.orphaned.length > 0 && (
                        <div>
                            <p className="mono-label text-(--warning) mb-2 flex items-center gap-1.5">
                                <AlertTriangle size={10} />
                                Orphaned (disk only — no DB record)
                            </p>
                            <div className="space-y-1">
                                {data.orphaned.map(o => (
                                    <div key={o.uuid} className="flex items-center justify-between px-3 py-2 bg-(--base-02) rounded-md border border-(--warning-border)/30">
                                        <span className="font-mono text-xs text-(--base-07) truncate">{o.uuid}</span>
                                        <div className="flex items-center gap-1.5 ml-3 shrink-0">
                                            <button
                                                onClick={() => setAssigningOrphan(o.uuid)}
                                                className="flex items-center gap-1.5 text-[11px] text-(--accent-light) hover:bg-(--base-03) px-2 py-1 rounded transition-colors"
                                            >
                                                <UserPlus size={12} />
                                                Assign
                                            </button>
                                            <button
                                                onClick={() => handleDelete(o.uuid)}
                                                disabled={deleting === o.uuid}
                                                className="flex items-center gap-1.5 text-[11px] text-(--error-light) hover:bg-(--error-ghost) px-2 py-1 rounded transition-colors"
                                            >
                                                {deleting === o.uuid ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                                Delete
                                            </button>
                                        </div>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {data.nodeOnline === false && (
                        <div className="alert alert-warning text-(--warning) text-xs">
                            <AlertTriangle size={13} className="shrink-0 mt-0.5" />
                            <span>Node is offline / no heartbeat - DB orphans cannot be reliably detected.</span>
                        </div>
                    )}

                    {data.nodeOnline !== false && data.missing.length > 0 && (
                        <div>
                            <p className="mono-label mb-2">
                                Missing from disk (DB-Leichen)
                            </p>
                            <div className="space-y-1">
                                {data.missing.map(m => (
                                    <div key={m.uuid} className="flex items-center justify-between px-3 py-2 bg-(--base-02) rounded-md border border-(--base-03)">
                                        <div className="min-w-0">
                                            <span className="text-sm text-(--base-07)">{m.serverName}</span>
                                            <span className="font-mono text-[10px] text-(--base-05) ml-2">{m.uuid}</span>
                                        </div>
                                        <button
                                            onClick={() => handleDeleteDbLeiche(m)}
                                            disabled={deletingDbId === m.id}
                                            className="flex items-center gap-1.5 text-[11px] text-(--error-light) hover:bg-(--error-ghost) px-2 py-1 rounded transition-colors ml-3 shrink-0"
                                        >
                                            {deletingDbId === m.id ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                            Remove from DB
                                        </button>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    {data.nodeOnline !== false && data.orphaned.length === 0 && data.missing.length === 0 && (
                        <p className="text-xs text-(--success-light) flex items-center gap-1.5">
                            <Check size={13} />
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
                                {pendingDelete.kind === 'orphan' ? 'Delete Orphaned Folder' : 'Remove Stale DB Entry'}
                            </h3>
                            <button onClick={() => setPendingDelete(null)} className="text-(--base-06) hover:text-(--error-light)">
                                <X size={18} />
                            </button>
                        </div>
                        <div className="modal-body space-y-3">
                            {deleteError && (
                                <div className="alert alert-error">
                                    {deleteError}
                                </div>
                            )}
                            {pendingDelete.kind === 'orphan' ? (
                                <>
                                    <p className="text-sm text-(--base-08)">
                                        Permanently delete this orphaned folder from{' '}
                                        <span className="font-mono text-(--base-09)">{node.name}</span>?
                                    </p>
                                    <div className="bg-(--base-02) border border-(--base-03) rounded-md px-3 py-2 font-mono text-xs text-(--base-07) break-all">
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
                                        The on-disk folder no longer exists on this node — the DB row is a leftover. Removing it cleans up dashboards and stats; nothing is touched on disk.
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
                                className="btn btn-danger disabled:opacity-40 inline-flex items-center gap-1.5"
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
