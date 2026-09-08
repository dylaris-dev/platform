"use client";

import React, { useState, useEffect } from 'react';
import { Loader2, X, UserPlus, User } from 'lucide-react';
import { assignOrphan, AssignOrphanInput, inspectOrphan } from '@/lib/api';
import { getUsers } from '@/lib/api/resources';
import type { User as UserType } from '@/lib/api';
import { sortUsersForPicker } from '@/lib/userOrder';
import { useAppData } from '@/lib/AppDataContext';
import { OrphanFileBrowser } from './OrphanFileBrowser';
import { Skeleton } from '@/components/Skeleton';

interface AssignOrphanModalProps {
    nodeId: number;
    uuid: string;
    onClose: () => void;
    onAssigned: () => void;
}

type OwnerMode = 'existing' | 'new';
type ActiveTab = 'assign' | 'files';

export function AssignOrphanModal({ nodeId, uuid, onClose, onAssigned }: AssignOrphanModalProps) {
    const { user } = useAppData();
    const currentUserId = user?.id;
    const [activeTab, setActiveTab] = useState<ActiveTab>('assign');

    // Form state
    const [name, setName] = useState(uuid);
    const [ownerMode, setOwnerMode] = useState<OwnerMode>('existing');
    const [users, setUsers] = useState<UserType[]>([]);
    const [usersLoading, setUsersLoading] = useState(false);
    const [usersError, setUsersError] = useState(false);
    const [selectedUserId, setSelectedUserId] = useState<string>('');
    const [newUsername, setNewUsername] = useState('');
    const [newPassword, setNewPassword] = useState('');
    const [memoryMb, setMemoryMb] = useState<number | ''>(1024);
    const [cpuLimit, setCpuLimit] = useState<number | ''>(1);

    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');

    // Pre-fill form from orphan metadata on mount. Fails silently — defaults stay.
    useEffect(() => {
        const prefill = async () => {
            try {
                const res = await inspectOrphan(nodeId, uuid);
                if (res?.success && res.metadata) {
                    const m = res.metadata as { name?: string; memory_mb?: number; cpu_limit?: number };
                    if (m.name) setName(m.name);
                    if (m.memory_mb && m.memory_mb > 0) setMemoryMb(m.memory_mb);
                    if (m.cpu_limit !== undefined && m.cpu_limit >= 0) setCpuLimit(m.cpu_limit);
                }
            } catch {
                // silent — modal works with defaults
            }
        };
        prefill();
    }, []); // eslint-disable-line react-hooks/exhaustive-deps

    useEffect(() => {
        const loadUsers = async () => {
            setUsersLoading(true);
            try {
                const res = await getUsers();
                setUsersLoading(false);
                if (res.success && Array.isArray(res.users)) {
                    setUsersError(false);
                    const ordered = sortUsersForPicker(res.users, currentUserId);
                    setUsers(ordered);
                    if (ordered.length > 0) {
                        setSelectedUserId(ordered[0].id);
                    }
                } else {
                    setUsersError(true);
                }
            } catch {
                setUsersLoading(false);
                setUsersError(true);
            }
        };
        loadUsers();
    }, []);

    const handleAssign = async () => {
        setError('');

        // Client-side validation
        if (!name.trim()) {
            setError('Name must not be empty.');
            return;
        }
        if (ownerMode === 'existing' && selectedUserId === '') {
            setError('Please select an existing user.');
            return;
        }
        if (ownerMode === 'new') {
            if (!newUsername.trim()) {
                setError('Username must not be empty.');
                return;
            }
            if (!newPassword.trim()) {
                setError('Password must not be empty.');
                return;
            }
        }
        if (!memoryMb || memoryMb <= 0) {
            setError('Memory must be greater than 0.');
            return;
        }
        if (cpuLimit === '' || cpuLimit < 0) {
            setError('CPU limit must be a valid number.');
            return;
        }

        const input: AssignOrphanInput = {
            node_id: nodeId,
            uuid,
            name: name.trim(),
            memory_mb: memoryMb as number,
            cpu_limit: cpuLimit as number,
            ...(ownerMode === 'existing'
                ? { owner_user_id: selectedUserId }
                : { new_user: { username: newUsername.trim(), password: newPassword } }
            ),
        };

        setSubmitting(true);
        const res = await assignOrphan(input);
        setSubmitting(false);

        if (res.success) {
            onAssigned();
            onClose();
        } else {
            setError(res.message ?? 'Assignment failed.');
        }
    };

    return (
        <div className="modal-overlay animate-fade-in">
            <div className="modal-panel w-full max-w-lg">
                {/* Header */}
                <div className="modal-header flex items-start justify-between">
                    <div>
                        <h3 className="modal-title flex items-center gap-2">
                            <UserPlus size={17} className="text-(--accent-light)" />
                            Assign orphan
                        </h3>
                        <p className="font-mono text-[11px] text-(--base-05) mt-1 break-all">{uuid}</p>
                    </div>
                    <button
                        onClick={onClose}
                        className="p-1 rounded hover:bg-(--base-03) text-(--base-06) mt-0.5 shrink-0"
                    >
                        <X size={16} />
                    </button>
                </div>

                {/* Tab switcher */}
                <div className="flex border-b border-(--base-03) px-6">
                    {(['assign', 'files'] as const).map(tab => (
                        <button
                            key={tab}
                            onClick={() => setActiveTab(tab)}
                            className={`px-4 py-2.5 text-xs font-medium transition-colors border-b-2 -mb-px ${
                                activeTab === tab
                                    ? 'border-(--accent) text-(--accent-light)'
                                    : 'border-transparent text-(--base-06) hover:text-(--base-08)'
                            }`}
                        >
                            {tab === 'assign' ? 'Assign' : 'Files'}
                        </button>
                    ))}
                </div>

                {/* Tab content */}
                {activeTab === 'files' ? (
                    <div className="modal-body">
                        <OrphanFileBrowser nodeId={nodeId} uuid={uuid} />
                    </div>
                ) : (
                    <>
                        <div className="modal-body space-y-4">
                            {error && (
                                <div className="alert alert-error text-sm">
                                    {error}
                                </div>
                            )}

                            {/* Name */}
                            <div>
                                <label className="mono-label mb-1.5 block">Name</label>
                                <input
                                    type="text"
                                    className="input-field w-full"
                                    value={name}
                                    onChange={e => setName(e.target.value)}
                                    placeholder="Server name"
                                />
                            </div>

                            {/* Owner mode toggle */}
                            <div>
                                <label className="mono-label mb-1.5 block">Owner</label>
                                <div className="flex rounded-md border border-(--base-03) overflow-hidden mb-3">
                                    <button
                                        type="button"
                                        onClick={() => setOwnerMode('existing')}
                                        className={`flex-1 flex items-center justify-center gap-1.5 py-2 text-xs font-medium transition-colors ${
                                            ownerMode === 'existing'
                                                ? 'bg-(--accent) text-white'
                                                : 'bg-(--base-02) text-(--base-07) hover:bg-(--base-03)'
                                        }`}
                                    >
                                        <User size={12} />
                                        Existing user
                                    </button>
                                    <button
                                        type="button"
                                        onClick={() => setOwnerMode('new')}
                                        className={`flex-1 flex items-center justify-center gap-1.5 py-2 text-xs font-medium transition-colors ${
                                            ownerMode === 'new'
                                                ? 'bg-(--accent) text-white'
                                                : 'bg-(--base-02) text-(--base-07) hover:bg-(--base-03)'
                                        }`}
                                    >
                                        <UserPlus size={12} />
                                        New user
                                    </button>
                                </div>

                                {ownerMode === 'existing' ? (
                                    usersLoading ? (
                                        <Skeleton className="h-9 w-full rounded" />
                                    ) : usersError ? (
                                        <div className="alert alert-error text-sm">
                                            Failed to load users.
                                        </div>
                                    ) : (
                                        <select
                                            className="input-field w-full"
                                            value={selectedUserId}
                                            onChange={e => setSelectedUserId(e.target.value)}
                                        >
                                            {users.length === 0 && (
                                                <option value="">- No users found -</option>
                                            )}
                                            {users.map(u => (
                                                <option key={u.id} value={u.id}>
                                                    {u.username}{u.email ? ` (${u.email})` : ''}
                                                </option>
                                            ))}
                                        </select>
                                    )
                                ) : (
                                    <div className="space-y-2.5">
                                        <input
                                            type="text"
                                            className="input-field w-full"
                                            placeholder="Username"
                                            value={newUsername}
                                            onChange={e => setNewUsername(e.target.value)}
                                            autoComplete="off"
                                        />
                                        <input
                                            type="password"
                                            className="input-field w-full"
                                            placeholder="Password"
                                            value={newPassword}
                                            onChange={e => setNewPassword(e.target.value)}
                                            autoComplete="new-password"
                                        />
                                    </div>
                                )}
                            </div>

                            {/* Resources */}
                            <div className="grid grid-cols-2 gap-3">
                                <div>
                                    <label className="mono-label mb-1.5 block">Memory (MB)</label>
                                    <input
                                        type="number"
                                        className="input-field w-full"
                                        value={memoryMb}
                                        min={1}
                                        step={256}
                                        onChange={e => setMemoryMb(e.target.value === '' ? '' : Number(e.target.value))}
                                        placeholder="1024"
                                    />
                                </div>
                                <div>
                                    <label className="mono-label mb-1.5 block">CPU limit</label>
                                    <input
                                        type="number"
                                        className="input-field w-full"
                                        value={cpuLimit}
                                        min={0}
                                        step={0.5}
                                        onChange={e => setCpuLimit(e.target.value === '' ? '' : Number(e.target.value))}
                                        placeholder="1"
                                    />
                                </div>
                            </div>
                        </div>

                        <div className="modal-footer">
                            <button
                                type="button"
                                onClick={onClose}
                                className="btn btn-secondary"
                                disabled={submitting}
                            >
                                Cancel
                            </button>
                            <button
                                type="button"
                                onClick={handleAssign}
                                disabled={submitting}
                                className="btn btn-primary disabled:opacity-40 inline-flex items-center gap-1.5"
                            >
                                {submitting && <Loader2 size={13} className="animate-spin" />}
                                Assign
                            </button>
                        </div>
                    </>
                )}
            </div>
        </div>
    );
}
