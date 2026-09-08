"use client";

import React, { useEffect, useState } from 'react';
import { LifeBuoy, Plus, Pencil, Trash2, X, Loader2, CircleCheck, CircleAlert } from 'lucide-react';
import {
    adminListTicketCategories, createTicketCategory, updateTicketCategory, deleteTicketCategory,
    TicketCategory, TicketPriority,
} from '@/lib/api/tickets';
import { SkeletonHeader, SkeletonTable } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';

const PRIORITIES: TicketPriority[] = ['low', 'normal', 'high', 'urgent'];

interface CategoryForm {
    name: string;
    description: string;
    requiresServer: boolean;
    defaultPriority: TicketPriority;
    defaultAssigneeTeam: string;
    color: string;
    enabled: boolean;
    position: number;
}

const emptyForm: CategoryForm = {
    name: '',
    description: '',
    requiresServer: false,
    defaultPriority: 'normal',
    defaultAssigneeTeam: '',
    color: '',
    enabled: true,
    position: 0,
};

export default function TicketCategoriesTab() {
    const [categories, setCategories] = useState<TicketCategory[]>([]);
    const [loading, setLoading] = useState(true);
    const [modal, setModal] = useState<{ mode: 'create' | 'edit'; original?: TicketCategory } | null>(null);
    const [form, setForm] = useState<CategoryForm>(emptyForm);
    const [busy, setBusy] = useState(false);
    const [deleting, setDeleting] = useState<number | null>(null);
    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);
    const [error, setError] = useState('');
    // Distinct from `error`, which belongs to the create/edit modal.
    const [loadError, setLoadError] = useState<string | null>(null);

    const showToast = (msg: string, ok = true) => {
        setToast({ msg, ok });
        setTimeout(() => setToast(null), 3000);
    };

    const load = async () => {
        const res = await adminListTicketCategories();
        // Dropping the failure left categories empty, which renders as "No
        // categories yet" - and Core answers 503 feature_disabled here when
        // tickets are off, so that misread was the normal view on such an
        // install, telling the admin to add one to enable a disabled feature.
        if (res.success) { setCategories(res.categories || []); setLoadError(null); }
        else setLoadError(res.message || 'Failed to load ticket categories.');
        setLoading(false);
    };

    useEffect(() => { load(); }, []);

    const openCreate = () => {
        setForm({ ...emptyForm, position: categories.length });
        setError('');
        setModal({ mode: 'create' });
    };

    const openEdit = (c: TicketCategory) => {
        setForm({
            name: c.name,
            description: c.description,
            requiresServer: c.requiresServer,
            defaultPriority: c.defaultPriority,
            defaultAssigneeTeam: c.defaultAssigneeTeam || '',
            color: c.color || '',
            enabled: c.enabled,
            position: c.position,
        });
        setError('');
        setModal({ mode: 'edit', original: c });
    };

    const closeModal = () => { setModal(null); setError(''); };

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!modal) return;
        setBusy(true);
        setError('');
        const payload = { ...form };
        const res = modal.mode === 'create'
            ? await createTicketCategory(payload)
            : modal.original ? await updateTicketCategory(modal.original.id, payload) : { success: false, message: 'No category' };
        setBusy(false);
        if (res.success) {
            closeModal();
            showToast(modal.mode === 'create' ? 'Category created.' : 'Category updated.');
            await load();
        } else {
            setError(res.message || 'Save failed.');
        }
    };

    const handleDelete = async (c: TicketCategory) => {
        // Deleting is safe for the tickets: each one keeps the category name it
        // was filed under as text. What it loses is the ability to be filtered
        // by that category, which is worth saying out loud.
        if (!(await confirmDialog({ title: 'Delete category', message: `Delete category "${c.name}"? Tickets already filed under it keep the name, but can no longer be filtered by it. Disable it instead to stop new tickets while keeping the filter.` }))) return;
        setDeleting(c.id);
        const res = await deleteTicketCategory(c.id);
        setDeleting(null);
        if (res.success) {
            showToast('Category deleted.');
            await load();
        } else {
            showToast(res.message || 'Delete failed.', false);
        }
    };

    if (loading) return (
        <div className="space-y-6 max-w-4xl">
            <SkeletonHeader />
            <SkeletonTable rows={4} cols={6} />
        </div>
    );

    return (
        <SettingsPage
            title="Ticket categories"
            icon={LifeBuoy}
            width="4xl"
            description={<>
                Categories users pick from when creating a ticket.{' '}
                <span className="font-mono">requires_server</span> gates the server-picker step.
                Deleting one keeps the name on tickets already filed under it. Disable
                instead to stop new tickets while keeping the category as a filter.
            </>}
            actions={
                <button type="button" onClick={openCreate} className="btn btn-primary inline-flex items-center gap-2 shrink-0">
                    <Plus size={14} /> Add category
                </button>
            }
        >
            <div className="card overflow-hidden">
                <table className="w-full text-sm">
                    <thead className="bg-(--base-03) text-(--base-06)">
                        <tr>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Name</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Server</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Default priority</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Default team</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Status</th>
                            <th className="text-right font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2 w-32">Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        {loadError ? (
                            <tr>
                                <td colSpan={6} className="text-center py-8 text-(--error) text-sm">
                                    {loadError}
                                </td>
                            </tr>
                        ) : categories.length === 0 && (
                            <tr>
                                <td colSpan={6} className="text-center py-8 text-(--base-06) text-sm">
                                    No categories yet. Add one to enable ticket creation.
                                </td>
                            </tr>
                        )}
                        {categories.map(c => (
                            <tr key={c.id} className="border-t border-(--base-03)">
                                <td className="px-4 py-3">
                                    <div className="flex items-center gap-2">
                                        {c.color && <span className="inline-block w-2.5 h-2.5 rounded-full" style={{ background: c.color }} />}
                                        <span className="font-medium">{c.name}</span>
                                    </div>
                                    {c.description && <p className="text-xs text-(--base-06) mt-0.5 line-clamp-1">{c.description}</p>}
                                </td>
                                <td className="px-4 py-3 text-xs">
                                    {c.requiresServer
                                        ? <span className="font-mono text-(--warning-light)">required</span>
                                        : <span className="text-(--base-06)">—</span>}
                                </td>
                                <td className="px-4 py-3 font-mono text-xs">{c.defaultPriority}</td>
                                <td className="px-4 py-3 font-mono text-xs">{c.defaultAssigneeTeam || <span className="text-(--base-06)">—</span>}</td>
                                <td className="px-4 py-3">
                                    {c.enabled
                                        ? <span className="text-xs text-(--success-light)">enabled</span>
                                        : <span className="text-xs text-(--base-06)">disabled</span>}
                                </td>
                                <td className="px-4 py-3 text-right">
                                    <div className="flex items-center justify-end gap-1">
                                        <button
                                            type="button"
                                            onClick={() => openEdit(c)}
                                            className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--base-09)"
                                            title="Edit"
                                        >
                                            <Pencil size={13} />
                                        </button>
                                        <button
                                            type="button"
                                            onClick={() => handleDelete(c)}
                                            disabled={deleting === c.id}
                                            className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost) disabled:opacity-40"
                                            title="Delete"
                                        >
                                            {deleting === c.id ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                                        </button>
                                    </div>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            {modal && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
                    <form onSubmit={handleSubmit} className="card w-full max-w-md mx-4 max-h-[90vh] flex flex-col">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title">{modal.mode === 'create' ? 'Add category' : `Edit ${modal.original?.name}`}</h3>
                            <button type="button" onClick={closeModal} className="text-(--base-07) hover:text-(--error-light)"><X size={18} /></button>
                        </div>
                        <div className="p-6 space-y-3 overflow-y-auto">
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Name</label>
                                <input
                                    type="text"
                                    value={form.name}
                                    onChange={e => setForm({ ...form, name: e.target.value })}
                                    maxLength={128}
                                    required
                                    autoFocus
                                    className="input-field w-full"
                                />
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Description</label>
                                <textarea
                                    value={form.description}
                                    onChange={e => setForm({ ...form, description: e.target.value })}
                                    rows={2}
                                    className="input-field w-full text-sm"
                                />
                                <p className="text-xs text-(--base-06)">Shown to the user when they pick this category.</p>
                            </div>
                            <label className="flex items-center gap-2 text-sm cursor-pointer">
                                <input
                                    type="checkbox"
                                    checked={form.requiresServer}
                                    onChange={e => setForm({ ...form, requiresServer: e.target.checked })}
                                    className="checkbox"
                                />
                                <span>Require server attachment</span>
                            </label>
                            <div className="grid grid-cols-2 gap-3">
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Default priority</label>
                                    <select
                                        value={form.defaultPriority}
                                        onChange={e => setForm({ ...form, defaultPriority: e.target.value as TicketPriority })}
                                        className="input-field w-full"
                                    >
                                        {PRIORITIES.map(p => <option key={p} value={p}>{p}</option>)}
                                    </select>
                                </div>
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Default team</label>
                                    <input
                                        type="text"
                                        value={form.defaultAssigneeTeam}
                                        onChange={e => setForm({ ...form, defaultAssigneeTeam: e.target.value })}
                                        placeholder="e.g. nodes"
                                        maxLength={64}
                                        className="input-field w-full"
                                    />
                                </div>
                            </div>
                            <div className="grid grid-cols-2 gap-3">
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Color</label>
                                    <div className="flex gap-2">
                                        <input
                                            type="color"
                                            value={form.color || '#7048C8'}
                                            onChange={e => setForm({ ...form, color: e.target.value })}
                                            className="w-10 h-10 rounded"
                                        />
                                        <input
                                            type="text"
                                            value={form.color}
                                            onChange={e => setForm({ ...form, color: e.target.value })}
                                            placeholder="(none)"
                                            className="input-mono flex-1 text-xs"
                                        />
                                    </div>
                                </div>
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Position</label>
                                    <input
                                        type="number"
                                        value={form.position}
                                        onChange={e => setForm({ ...form, position: Number(e.target.value) || 0 })}
                                        className="input-field w-full"
                                    />
                                </div>
                            </div>
                            <label className="flex items-center gap-2 text-sm cursor-pointer pt-1">
                                <input
                                    type="checkbox"
                            className="checkbox"
                                    checked={form.enabled}
                                    onChange={e => setForm({ ...form, enabled: e.target.checked })}
                                />
                                <span>Enabled (visible to users)</span>
                            </label>
                            {error && (
                                <p className="text-xs text-(--error-light) flex items-center gap-1">
                                    <CircleAlert size={12} /> {error}
                                </p>
                            )}
                        </div>
                        <div className="modal-footer flex justify-end gap-2">
                            <button type="button" onClick={closeModal} className="btn btn-secondary">Cancel</button>
                            <button type="submit" disabled={busy} className="btn btn-primary inline-flex items-center gap-2">
                                {busy && <Loader2 size={14} className="animate-spin" />}
                                {modal.mode === 'create' ? 'Create' : 'Save'}
                            </button>
                        </div>
                    </form>
                </div>
            )}

            {toast && (
                <div className={`fixed bottom-4 right-4 px-4 py-3 rounded-md shadow-lg text-sm font-medium ${
                    toast.ok ? 'bg-(--success-ghost) text-(--success-light)' : 'bg-(--error-ghost) text-(--error-light)'
                }`}>
                    <span className="inline-flex items-center gap-2">
                        {toast.ok ? <CircleCheck size={14} /> : <CircleAlert size={14} />}
                        {toast.msg}
                    </span>
                </div>
            )}
        </SettingsPage>
    );
}
