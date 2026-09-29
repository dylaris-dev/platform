"use client";

import React, { useEffect, useState } from 'react';
import { MessageSquareQuote, Plus, Pencil, Trash2, X, Loader2, CircleCheck, CircleAlert } from 'lucide-react';
import {
    adminListCannedResponses, createCannedResponse, updateCannedResponse, deleteCannedResponse,
    listTicketCategories,
    CannedResponse, TicketCategory,
} from '@/lib/api/tickets';
import { SkeletonHeader, SkeletonTable } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import ModalPanel from '@/components/ui/ModalPanel';

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
    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);
    const [error, setError] = useState('');

    const showToast = (msg: string, ok = true) => {
        setToast({ msg, ok });
        setTimeout(() => setToast(null), 2800);
    };

    const load = async () => {
        const [a, b] = await Promise.all([adminListCannedResponses(), listTicketCategories()]);
        if (a.success) setResponses(a.responses || []);
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
        <div className="space-y-6 max-w-4xl">
            <SkeletonHeader />
            <SkeletonTable rows={5} cols={4} showHeader />
        </div>
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

            <div className="card overflow-hidden">
                <table className="w-full text-sm">
                    <thead className="bg-(--base-03) text-(--base-06)">
                        <tr>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Name</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Category</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Preview</th>
                            <th className="text-right font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2 w-32">Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        {responses.length === 0 && (
                            <tr>
                                <td colSpan={4} className="text-center py-8 text-(--base-06) text-sm">
                                    No canned responses yet. Add some to speed up replies.
                                </td>
                            </tr>
                        )}
                        {responses.map(c => {
                            const cat = c.categoryId != null ? categories.find(x => x.id === c.categoryId) : undefined;
                            return (
                                <tr key={c.id} className="border-t border-(--base-03)">
                                    <td className="px-4 py-3 font-medium">{c.name}</td>
                                    <td className="px-4 py-3 text-xs">
                                        {cat ? <span className="font-mono">{cat.name}</span> : <span className="text-(--base-06)">global</span>}
                                    </td>
                                    <td className="px-4 py-3 text-xs text-(--base-06) line-clamp-1 max-w-xs">{c.body}</td>
                                    <td className="px-4 py-3 text-right">
                                        <div className="flex items-center justify-end gap-1">
                                            <button type="button" onClick={() => openEdit(c)}
                                                className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--base-09)" title="Edit">
                                                <Pencil size={13} />
                                            </button>
                                            <button type="button" onClick={() => handleDelete(c)}
                                                disabled={deleting === c.id}
                                                className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost) disabled:opacity-40" title="Delete">
                                                {deleting === c.id ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                                            </button>
                                        </div>
                                    </td>
                                </tr>
                            );
                        })}
                    </tbody>
                </table>
            </div>

            {modal && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
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
                                <select value={form.categoryId}
                                    onChange={e => setForm({ ...form, categoryId: e.target.value === '' ? '' : Number(e.target.value) })}
                                    className="input-field w-full">
                                    <option value="">— Global (any category) —</option>
                                    {categories.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
                                </select>
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
