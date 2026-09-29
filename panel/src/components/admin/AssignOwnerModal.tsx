"use client";

import React, { useState } from 'react';
import { Loader2, X, Check } from 'lucide-react';
import { updateServerOwner, AdminServer, User } from '@/lib/api';
import ModalPanel from '@/components/ui/ModalPanel';

interface AssignOwnerModalProps {
    server: AdminServer;
    users: User[];
    onClose: () => void;
    onAssigned: () => void;
}

export function AssignOwnerModal({ server, users, onClose, onAssigned }: AssignOwnerModalProps) {
    const [search, setSearch] = useState('');
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState('');

    const filtered = users.filter(u =>
        u.username.toLowerCase().includes(search.toLowerCase()) ||
        u.email?.toLowerCase().includes(search.toLowerCase())
    );

    const assign = async (userId: string) => {
        setLoading(true);
        setError('');
        const res = await updateServerOwner(server.id, userId);
        setLoading(false);
        if (res.success) {
            onAssigned();
            onClose();
        } else {
            setError(res.message ?? 'Failed to assign owner');
        }
    };

    return (
        <div className="modal-overlay animate-fade-in">
            <ModalPanel className="modal-panel w-full max-w-md">
                <div className="modal-header">
                    <h3 className="modal-title">Assign Owner</h3>
                    <button onClick={onClose} className="p-1 rounded hover:bg-(--base-03) text-(--base-06)">
                        <X size={16} />
                    </button>
                </div>
                <div className="modal-body">
                    <p className="text-xs text-(--base-06) mb-3">
                        Assign <span className="text-(--base-08) font-medium">{server.name}</span> to a user.
                    </p>
                    {error && (
                        <div className="alert alert-error mb-3">
                            {error}
                        </div>
                    )}
                    <input
                        type="text"
                        placeholder="Search users..."
                        value={search}
                        onChange={e => setSearch(e.target.value)}
                        className="input-field w-full mb-3"
                    />
                    <div className="max-h-60 overflow-y-auto space-y-1">
                        {filtered.map(u => (
                            <button
                                key={u.id}
                                onClick={() => assign(u.id)}
                                disabled={loading}
                                className="w-full flex items-center justify-between px-3 py-2 rounded-md hover:bg-(--base-03) text-left transition-colors group"
                            >
                                <div>
                                    <p className="text-sm font-medium text-(--base-09)">{u.username}</p>
                                    {u.email && <p className="text-[11px] text-(--base-06)">{u.email}</p>}
                                </div>
                                {loading
                                    ? <Loader2 size={14} className="animate-spin text-(--base-05)" />
                                    : <Check size={14} className="text-(--accent-light) opacity-0 group-hover:opacity-100 transition-opacity" />
                                }
                            </button>
                        ))}
                        {filtered.length === 0 && (
                            <p className="text-xs text-(--base-05) text-center py-4">No users found</p>
                        )}
                    </div>
                </div>
            </ModalPanel>
        </div>
    );
}
