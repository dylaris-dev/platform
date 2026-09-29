"use client";

import { useState } from 'react';
import { Shield, Plus, Pencil, Trash2, X, AlertTriangle } from 'lucide-react';
import type { CatalogScope } from '@/lib/api/authzCatalog';
import {
    createServerRole, updateServerRole, deleteServerRole,
    type ServerRole,
} from '@/lib/api/serverRoles';
import CapabilityPicker from '@/components/access/CapabilityPicker';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import ModalPanel from '@/components/ui/ModalPanel';

// Advanced-mode owner UI: custom per-server capability bundles. Bundles
// SERVER + OWNER scope capabilities only (matches the backend
// validateServerOwnerCaps rule); assignment of a role to a friend happens in
// the grant form (AccessGrantForm), not here.
//
// The role list lives in the parent page (access/page.tsx) so this component
// and the grant form share one up-to-date copy; onRolesChanged() asks the
// parent to re-fetch after a create/update/delete.

interface AccessServerRolesProps {
    catalog: CatalogScope[];
    roles: ServerRole[];
    loading: boolean;
    onRolesChanged: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}

export default function AccessServerRoles({ catalog, roles, loading, onRolesChanged, showToast }: AccessServerRolesProps) {
    const [editing, setEditing] = useState<ServerRole | null>(null);
    const [savingRole, runSave] = useBusy();
    const [deletingRole, runDelete] = useBusy();
    const [showEditor, setShowEditor] = useState(false);
    const [name, setName] = useState('');
    const [caps, setCaps] = useState<string[]>([]);

    const [deleting, setDeleting] = useState<ServerRole | null>(null);

    const openCreate = () => {
        setEditing(null);
        setName('');
        setCaps([]);
        setShowEditor(true);
    };

    const openEdit = (role: ServerRole) => {
        setEditing(role);
        setName(role.name);
        setCaps(role.capabilities);
        setShowEditor(true);
    };

    const handleSave = async () => {
        const trimmed = name.trim();
        if (!trimmed) { showToast('Name required', false); return; }
        if (caps.length === 0) { showToast('Pick at least one capability', false); return; }
        const res = editing
            ? await updateServerRole(editing.id, trimmed, caps)
            : await createServerRole(trimmed, caps);
        if (res.success) {
            showToast(editing ? 'Role updated.' : 'Role created.', true);
            setShowEditor(false);
            onRolesChanged();
        } else {
            showToast(res.message || (editing ? 'Update failed' : 'Create failed'), false);
        }
    };

    const handleDelete = async () => {
        if (!deleting) return;
        const res = await deleteServerRole(deleting.id);
        setDeleting(null);
        if (res.success) {
            showToast('Role deleted.', true);
            onRolesChanged();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    return (
        <section className="mb-4">
            <div className="flex items-center gap-3 mb-2">
                <h2 className="h-section">Server roles</h2>
                <div className="ml-auto">
                    <button onClick={openCreate} className="btn btn-primary btn-sm">
                        <Plus size={12} />
                        New role
                    </button>
                </div>
            </div>

            {loading ? (
                <SkeletonList rows={2} />
            ) : roles.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <Shield size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No server roles yet.</p>
                </div>
            ) : (
                <div className="space-y-2">
                    {roles.map(role => (
                        <article key={role.id} className="card p-3 flex items-center gap-3">
                            <div className="min-w-0 flex-1">
                                <div className="flex items-center gap-2 flex-wrap">
                                    <span className="font-medium text-sm text-(--base-09)">{role.name}</span>
                                    <span className="mono-label bg-(--base-03) text-(--base-07) px-1.5 rounded-sm">
                                        {role.capabilities.length} capabilit{role.capabilities.length === 1 ? 'y' : 'ies'}
                                    </span>
                                </div>
                            </div>
                            <button onClick={() => openEdit(role)} className="btn btn-secondary btn-sm shrink-0">
                                <Pencil size={12} />
                                Edit
                            </button>
                            <button onClick={() => setDeleting(role)} className="btn btn-danger btn-sm shrink-0">
                                <Trash2 size={12} />
                                Delete
                            </button>
                        </article>
                    ))}
                </div>
            )}

            {showEditor && (
                <div className="modal-overlay animate-fade-in" onClick={() => setShowEditor(false)}>
                    <ModalPanel onClose={() => setShowEditor(false)} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <Shield size={16} />
                                {editing ? `Edit ${editing.name}` : 'New server role'}
                            </h3>
                            <button onClick={() => setShowEditor(false)} className="text-(--base-06)"><X size={16} /></button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div>
                                <label className="input-label">Name</label>
                                <input
                                    type="text"
                                    value={name}
                                    onChange={e => setName(e.target.value)}
                                    className="input-field w-full"
                                    placeholder="moderator"
                                    maxLength={64}
                                />
                            </div>
                            <div>
                                <label className="input-label">Capabilities</label>
                                <div className="mt-1">
                                    <CapabilityPicker
                                        catalog={catalog}
                                        scopes={['server', 'owner']}
                                        selected={caps}
                                        onChange={setCaps}
                                    />
                                </div>
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setShowEditor(false)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runSave(handleSave)} disabled={savingRole} className="btn btn-primary disabled:opacity-40">{editing ? 'Save' : 'Create role'}</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {deleting && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeleting(null)}>
                    <ModalPanel onClose={() => setDeleting(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                Delete {deleting.name}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Friends currently assigned this role will lose its capabilities immediately.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeleting(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingRole} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </section>
    );
}
