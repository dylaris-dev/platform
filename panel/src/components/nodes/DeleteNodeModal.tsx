'use client';

import { useEffect, useState } from 'react';
import { AlertTriangle, X } from 'lucide-react';
import { getNodeServers, forceDeleteNode } from '@/lib/api';

/**
 * Deleting a node, and everything on it.
 *
 * This lived on the Infrastructure page, which is otherwise a read-only view of
 * how the fleet is doing. Deleting a machine is not a reading, and having the
 * one irreversible action sit among the graphs while every other node control
 * lived in Settings meant an operator had to know two screens to manage one
 * node. It is now only reachable from Settings -> Nodes.
 *
 * The list of servers is fetched rather than passed in: the caller's copy comes
 * from a poll and can be seconds old, and "and all 3 servers on it" is the
 * sentence someone reads before typing the name. It is worth one request.
 */
export interface DeletableNode {
    id: number;
    name: string;
}

interface NodeServer {
    id: number;
    uuid: string;
    name: string;
    status: string;
}

export default function DeleteNodeModal({
    node,
    onClose,
    onDeleted,
    onError,
}: {
    node: DeletableNode;
    onClose: () => void;
    onDeleted: (name: string) => void;
    onError: (msg: string) => void;
}) {
    const [servers, setServers] = useState<NodeServer[] | null>(null);
    const [confirmName, setConfirmName] = useState('');
    const [deleting, setDeleting] = useState(false);

    useEffect(() => {
        let cancelled = false;
        getNodeServers(node.id)
            .then(res => { if (!cancelled) setServers(res?.servers || []); })
            .catch(() => { if (!cancelled) setServers([]); });
        return () => { cancelled = true; };
    }, [node.id]);

    const handleDelete = async () => {
        setDeleting(true);
        try {
            const res = await forceDeleteNode(node.id);
            if (res?.success) onDeleted(node.name);
            else onError(res?.message || 'Delete failed');
        } catch {
            onError('Delete failed');
        } finally {
            setDeleting(false);
        }
    };

    const count = servers?.length ?? 0;

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <div className="flex items-center gap-2">
                        <AlertTriangle size={18} className="text-(--error)" />
                        <h3 className="modal-title">Delete node</h3>
                    </div>
                    <button onClick={onClose} className="p-1 text-(--base-06) hover:text-(--base-09) transition-colors" aria-label="Close">
                        <X size={16} />
                    </button>
                </div>

                <div className="modal-body space-y-4">
                    <div className="rounded-md bg-(--error)/10 border border-(--error)/30 p-3">
                        <p className="text-sm text-(--error)">
                            This permanently deletes node <strong>&quot;{node.name}&quot;</strong>
                            {count > 0 && (
                                <> and all <strong>{count}</strong> server{count !== 1 ? 's' : ''} on it</>
                            )}. This cannot be undone.
                        </p>
                    </div>

                    {servers === null ? (
                        <p className="text-xs text-(--base-06)">Checking what is on this node...</p>
                    ) : count > 0 && (
                        <div>
                            <p className="mono-label mb-2">Servers on this node</p>
                            <div className="rounded-md border border-(--base-03) divide-y divide-(--base-03) max-h-40 overflow-y-auto">
                                {servers.map(srv => (
                                    <div key={srv.id} className="px-3 py-2 flex items-center justify-between gap-2">
                                        <div className="min-w-0">
                                            <p className="text-sm text-(--base-09) truncate">{srv.name}</p>
                                            <p className="text-[10px] font-mono text-(--base-05) truncate">{srv.uuid}</p>
                                        </div>
                                        <span className="text-[10px] font-mono uppercase text-(--base-06) shrink-0">{srv.status}</span>
                                    </div>
                                ))}
                            </div>
                        </div>
                    )}

                    <div>
                        <label className="mono-label block mb-1.5" htmlFor="delete-node-confirm">
                            Type &quot;{node.name}&quot; to confirm
                        </label>
                        <input
                            id="delete-node-confirm"
                            type="text"
                            value={confirmName}
                            onChange={e => setConfirmName(e.target.value)}
                            placeholder={node.name}
                            className="input-field w-full"
                            autoComplete="off"
                        />
                    </div>
                </div>

                <div className="modal-footer flex justify-end gap-2">
                    <button type="button" onClick={onClose} className="btn btn-sm">Cancel</button>
                    <button
                        type="button"
                        onClick={handleDelete}
                        // Waiting for the server list is deliberate: the count is
                        // part of what is being confirmed.
                        disabled={confirmName !== node.name || deleting || servers === null}
                        className="btn btn-sm btn-danger"
                    >
                        {deleting ? 'Deleting...' : 'Delete node'}
                    </button>
                </div>
            </div>
        </div>
    );
}
