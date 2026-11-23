"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import React, { useEffect, useState } from 'react';
import { MessageSquareQuote, Plus, Pencil, Trash2, X, Loader2, CircleAlert, MoreHorizontal } from 'lucide-react';
import {
    adminListCannedResponses, createCannedResponse, updateCannedResponse, deleteCannedResponse,
    listTicketCategories,
    CannedResponse, TicketCategory,
} from '@/lib/api/tickets';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';

const TEMPLATE_VARS = ['{{user_name}}', '{{ticket_id}}', '{{server_name}}', '{{actor_name}}'];

interface FormState {
    name: string;
    body: string;
    categoryId: number | '';
}

const emptyForm: FormState = { name: '', body: '', categoryId: '' };

export default function CannedResponsesTab() {
    const [responses, setResponses] = useState<CannedResponse[]>([]);
    const [categories, setCategories] = useState<TicketCategory[]>([]);
    const [loading, setLoading] = useState(true);
    const [modal, setModal] = useState<{ mode: 'create' | 'edit'; original?: CannedResponse } | null>(null);
    const [form, setForm] = useState<FormState>(emptyForm);
    const [busy, setBusy] = useState(false);
    const [deleting, setDeleting] = useState<number | null>(null);

    const [error, setError] = useState('');
    const [loadError, setLoadError] = useState<string | null>(null);

    const showToast = (msg: string, ok = true) => {
        notifyToast(msg, ok);

    };

    const load = async () => {
        const [a, b] = await Promise.all([adminListCannedResponses(), listTicketCategories()]);
        if (a.success) { setResponses(a.responses || []); setLoadError(null); }
        else setLoadError(a.message || 'Failed to load canned responses.');
        if (b.success) setCategories(b.categories || []);
        setLoading(false);
    };

    useEffect(() => { load(); }, []);

    const openCreate = () => {
        setForm(emptyForm);
        setError('');
        setModal({ mode: 'create' });
    };

    const openEdit = (c: CannedResponse) => {
        setForm({ name: c.name, body: c.body, categoryId: c.categoryId ?? '' });
        setError('');
        setModal({ mode: 'edit', original: c });
    };

    const close = () => { setModal(null); setError(''); };

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!modal) return;
        setBusy(true);
        setError('');
        const payload = {
            name: form.name.trim(),
            body: form.body,
            categoryId: form.categoryId === '' ? null : Number(form.categoryId),
        };
        const res = modal.mode === 'create'
            ? await createCannedResponse(payload)
            : modal.original ? await updateCannedResponse(modal.original.id, payload) : { success: false, message: 'No original' };
        setBusy(false);
        if (res.success) {
            close();
            showToast(modal.mode === 'create' ? 'Response created.' : 'Response updated.');
            await load();
        } else {
            setError(res.message || 'Save failed.');
        }
    };

    const handleDelete = async (c: CannedResponse) => {
        if (!(await confirmDialog({ title: 'Delete canned response', message: `Delete "${c.name}"?` }))) return;
        setDeleting(c.id);
        const res = await deleteCannedResponse(c.id);
        setDeleting(null);
        if (res.success) {
            showToast('Deleted.');
            await load();
        } else {
            showToast(res.message || 'Delete failed.', false);
        }
    };

    if (loading) return (
        <SettingsPage title="Canned responses" loading skeletonCards={2} width="4xl">{null}</SettingsPage>
    );

    return (
        <SettingsPage
            title="Canned responses"
            icon={MessageSquareQuote}
            width="4xl"
            description={<>
                Pre-written snippets support staff can insert into ticket replies. Optional
                category binding limits each snippet to tickets in that category. Template
                variables: <span className="font-mono">{TEMPLATE_VARS.join(' · ')}</span>
            </>}
            actions={
                <button type="button" onClick={openCreate} className="btn btn-primary inline-flex items-center gap-2 shrink-0">
                    <Plus size={14} /> Add response
                </button>
            }
        >
            {loadError ? (
                <EmptyState tone="error" icon={<CircleAlert size={20} />} title="Canned responses could not be loaded" description={loadError} />
            ) : responses.length === 0 ? (
                <EmptyState
                    icon={<MessageSquareQuote size={20} />}
                    title="No canned responses yet"
                    description="Add some to speed up replies."
                    action={<button type="button" onClick={openCreate} className="btn btn-primary btn-sm inline-flex items-center gap-1.5"><Plus size={13} /> Add response</button>}
                />
            ) : (
                <div className="srv-surface">
                    <RowList label="Canned responses">
                        {responses.map(c => {
                            const cat = c.categoryId != null ? categories.find(x => x.id === c.categoryId) : undefined;
                            return (
                                <Row
                                    key={c.id}
                                    title={c.name}
                                    meta={c.body}
                                    trailing={cat ? <span className="font-mono">{cat.name}</span> : 'Global'}
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
                            );
                        })}
                    </RowList>
                </div>
            )}

            {modal && (
                <div className="modal-overlay">
                    <ModalPanel as="form" onSubmit={handleSubmit} className="card w-full max-w-lg mx-4 max-h-[90vh] flex flex-col">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title">{modal.mode === 'create' ? 'Add canned response' : `Edit ${modal.original?.name}`}</h3>
                            <button type="button" onClick={close} className="text-(--base-07) hover:text-(--error-light)"><X size={18} /></button>
                        </div>
                        <div className="p-6 space-y-3 overflow-y-auto">
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Name</label>
                                <input type="text" value={form.name}
                                    onChange={e => setForm({ ...form, name: e.target.value })}
                                    maxLength={128} required autoFocus className="input-field w-full" />
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Category (optional)</label>
                                <Select ariaLabel="Category"
                                    value={String(form.categoryId)}
                                    onChange={v => setForm({ ...form, categoryId: v === '' ? '' : Number(v) })}
                                    options={[{ value: '', label: '— Global (any category) —' }, ...categories.map(c => ({ value: String(c.id), label: c.name }))]} />
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Body</label>
                                <textarea value={form.body}
                                    onChange={e => setForm({ ...form, body: e.target.value })}
                                    rows={8} maxLength={10000} required className="input-field w-full text-sm" />
                                <p className="text-xs text-(--base-06)">Use the variables above. They get expanded when support inserts the snippet.</p>
                            </div>
                            {error && (
                                <p className="text-xs text-(--error-light) flex items-center gap-1">
                                    <CircleAlert size={12} /> {error}
                                </p>
                            )}
                        </div>
                        <div className="modal-footer flex justify-end gap-2">
                            <button type="button" onClick={close} className="btn btn-secondary">Cancel</button>
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
