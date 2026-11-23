"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import React, { useEffect, useState } from 'react';
import { LifeBuoy, Plus, Pencil, Trash2, X, Loader2, CircleAlert, MoreHorizontal } from 'lucide-react';
import {
    adminListTicketCategories, createTicketCategory, updateTicketCategory, deleteTicketCategory,
    TicketCategory, TicketPriority,
} from '@/lib/api/tickets';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';

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

    const [error, setError] = useState('');
    // Distinct from `error`, which belongs to the create/edit modal.
    const [loadError, setLoadError] = useState<string | null>(null);

    const showToast = (msg: string, ok = true) => {
        notifyToast(msg, ok);

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
        <SettingsPage title="Ticket categories" loading skeletonCards={2} width="4xl">{null}</SettingsPage>
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
            {loadError ? (
                <EmptyState tone="error" icon={<CircleAlert size={20} />} title="Categories could not be loaded" description={loadError} />
            ) : categories.length === 0 ? (
                <EmptyState
                    icon={<LifeBuoy size={20} />}
                    title="No categories yet"
                    description="Add one to enable ticket creation."
                    action={<button type="button" onClick={openCreate} className="btn btn-primary btn-sm inline-flex items-center gap-1.5"><Plus size={13} /> Add category</button>}
                />
            ) : (
                <div className="srv-surface">
                    <RowList label="Ticket categories">
                        {categories.map(c => (
                            <Row
                                key={c.id}
                                leading={<span className="inline-block size-2.5 rounded-full" style={{ background: c.color || 'var(--base-05)' }} aria-hidden="true" />}
                                title={c.name}
                                meta={<>
                                    {c.description && <>{c.description} &middot; </>}
                                    <span className="font-mono">{c.defaultPriority}</span>
                                    {c.defaultAssigneeTeam && <> &middot; team <span className="font-mono">{c.defaultAssigneeTeam}</span></>}
                                </>}
                                trailing={
                                    <span className="flex items-center gap-3">
                                        {c.requiresServer && <span className="text-(--warning-light)">Server required</span>}
                                        <span className={c.enabled ? 'text-(--success-light)' : 'text-(--base-06)'}>{c.enabled ? 'Enabled' : 'Disabled'}</span>
                                    </span>
                                }
                                actions={
                                    deleting === c.id
                                        ? <Loader2 size={14} className="mx-2 animate-spin text-(--base-06)" aria-label="Deleting" />
                                        : (
                                            <Menu
                                                trigger={<button type="button" className="app-icon-btn" aria-label={`Actions for ${c.name}`}><MoreHorizontal size={16} /></button>}
                                                items={[
                                                    { label: 'Edit', icon: <Pencil size={14} />, onSelect: () => openEdit(c) },
                                                    { type: 'separator' },
                                                    { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDelete(c) },
                                                ]}
                                            />
                                        )
                                }
                            />
                        ))}
                    </RowList>
                </div>
            )}

            {modal && (
                <div className="modal-overlay">
                    <ModalPanel as="form" onSubmit={handleSubmit} className="card w-full max-w-md mx-4 max-h-[90vh] flex flex-col">
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
                            <Checkbox
                                checked={form.requiresServer}
                                onChange={c => setForm({ ...form, requiresServer: c })}
                                label="Require server attachment"
                            />
                            <div className="grid grid-cols-2 gap-3">
                                <div className="flex flex-col gap-[5px]">
                                    <label className="input-label">Default priority</label>
                                    <Select
                                        ariaLabel="Default priority"
                                        value={form.defaultPriority}
                                        onChange={v => setForm({ ...form, defaultPriority: v as TicketPriority })}
                                        options={PRIORITIES.map(p => ({ value: p, label: p }))}
                                    />
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
                            <Checkbox
                                className="pt-1"
                                checked={form.enabled}
                                onChange={c => setForm({ ...form, enabled: c })}
                                label="Enabled (visible to users)"
                            />
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
                    </ModalPanel>
                </div>
            )}


        </SettingsPage>
    );
}
