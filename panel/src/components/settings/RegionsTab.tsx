"use client";

import React, { useState, useEffect, useCallback } from 'react';
import { adminListRegions, createRegion, updateRegion, deleteRegion, Region } from '@/lib/api/regions';
import { Globe, Plus, Pencil, Trash2, X, CircleCheck, CircleAlert, Loader2 } from 'lucide-react';
import { SkeletonHeader, SkeletonTable } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import SettingsPage from '@/components/settings/SettingsPage';
import HelpTip from '@/components/ui/HelpTip';
import ModalPanel from '@/components/ui/ModalPanel';

interface RegionFormState {
    id: string;
    displayName: string;
    enabled: boolean;
    color: string;
}

const emptyForm: RegionFormState = { id: '', displayName: '', enabled: true, color: '' };

export default function RegionsTab() {
    const [regions, setRegions] = useState<Region[]>([]);
    const [loading, setLoading] = useState(true);
    const [modal, setModal] = useState<{ mode: 'create' | 'edit'; original?: Region } | null>(null);
    const [form, setForm] = useState<RegionFormState>(emptyForm);
    const [busy, setBusy] = useState(false);
    const [deleting, setDeleting] = useState<string | null>(null);
    const [toast, setToast] = useState<{ msg: string; ok: boolean } | null>(null);
    const [error, setError] = useState('');
    // Distinct from `error`, which belongs to the create/edit modal.
    const [loadError, setLoadError] = useState<string | null>(null);

    const showToast = (msg: string, ok = true) => {
        setToast({ msg, ok });
        setTimeout(() => setToast(null), 3500);
    };

    const load = useCallback(async () => {
        const res = await adminListRegions();
        // Dropping the failure left regions empty, which the table renders as
        // "No regions configured" - a claim about the config, not about a call
        // that never answered.
        if (res.success) { setRegions(res.regions || []); setLoadError(null); }
        else setLoadError(res.message || 'Failed to load regions.');
        setLoading(false);
    }, []);

    useEffect(() => { load(); }, [load]);

    const openCreate = () => {
        setForm(emptyForm);
        setError('');
        setModal({ mode: 'create' });
    };

    const openEdit = (r: Region) => {
        setForm({ id: r.id, displayName: r.displayName, enabled: r.enabled, color: r.color || '' });
        setError('');
        setModal({ mode: 'edit', original: r });
    };

    const closeModal = () => {
        setModal(null);
        setError('');
    };

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (!modal) return;
        setBusy(true);
        setError('');

        const res = modal.mode === 'create'
            ? await createRegion({ id: form.id, displayName: form.displayName, enabled: form.enabled, color: form.color })
            : await updateRegion(form.id, { displayName: form.displayName, enabled: form.enabled, color: form.color });

        setBusy(false);
        if (res.success) {
            closeModal();
            showToast(modal.mode === 'create' ? 'Region created.' : 'Region updated.');
            await load();
        } else {
            setError(res.message || 'Save failed.');
        }
    };

    const handleDelete = async (id: string) => {
        if (id === 'default') {
            showToast('Cannot delete the default region.', false);
            return;
        }
        if (!(await confirmDialog({ title: 'Delete region', message: `Delete region "${id}"? Servers and nodes in this region must be reassigned first.` }))) return;
        setDeleting(id);
        const res = await deleteRegion(id);
        setDeleting(null);
        if (res.success) {
            showToast('Region deleted.');
            await load();
        } else {
            showToast(res.message || 'Delete failed.', false);
        }
    };

    if (loading) return (
        <div className="space-y-6 max-w-4xl">
            <SkeletonHeader />
            <SkeletonTable rows={4} cols={5} />
        </div>
    );

    return (
        <SettingsPage
            title="Regions"
            icon={Globe}
            width="4xl"
            description={<>
                Geographic deployment regions. Single-region setups have one region called{' '}
                <span className="font-mono">default</span>. Multi-region setups add regions like{' '}
                <span className="font-mono">eu</span> or <span className="font-mono">us-east</span>,
                used as the value of <span className="font-mono">nodes.region</span> and{' '}
                <span className="font-mono">servers.region</span>.
            </>}
            actions={
                <button type="button" onClick={openCreate} className="btn btn-primary inline-flex items-center gap-2 shrink-0">
                    <Plus size={14} /> Add Region
                </button>
            }
        >
            <div className="card overflow-hidden">
                <table className="w-full text-sm">
                    <thead className="bg-(--base-03) text-(--base-06)">
                        <tr>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">ID</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Display name</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Enabled</th>
                            <th className="text-left font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2">Color</th>
                            <th className="text-right font-mono text-[10px] uppercase tracking-[0.08em] px-4 py-2 w-32">Actions</th>
                        </tr>
                    </thead>
                    <tbody>
                        {loadError ? (
                            <tr>
                                <td colSpan={5} className="text-center py-8 text-(--error-light) text-sm">
                                    {loadError}
                                </td>
                            </tr>
                        ) : regions.length === 0 && (
                            <tr>
                                <td colSpan={5} className="text-center py-8 text-(--base-06) text-sm">
                                    No regions configured. Add one to get started.
                                </td>
                            </tr>
                        )}
                        {regions.map(r => (
                            <tr key={r.id} className="border-t border-(--base-03)">
                                <td className="px-4 py-3 font-mono text-(--accent-light)">{r.id}</td>
                                <td className="px-4 py-3">{r.displayName}</td>
                                <td className="px-4 py-3">
                                    {r.enabled
                                        ? <span className="inline-flex items-center gap-1 text-(--success-light) text-xs"><CircleCheck size={12} /> enabled</span>
                                        : <span className="inline-flex items-center gap-1 text-(--base-06) text-xs"><CircleAlert size={12} /> disabled</span>}
                                </td>
                                <td className="px-4 py-3">
                                    {r.color
                                        ? <span className="inline-flex items-center gap-2 text-xs"><span className="inline-block w-3 h-3 rounded" style={{ background: r.color }} /> {r.color}</span>
                                        : <span className="text-(--base-06) text-xs">—</span>}
                                </td>
                                <td className="px-4 py-3">
                                    <div className="flex items-center justify-end gap-1">
                                        <button
                                            type="button"
                                            onClick={() => openEdit(r)}
                                            title="Edit"
                                            className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--base-09)"
                                        >
                                            <Pencil size={13} />
                                        </button>
                                        <button
                                            type="button"
                                            onClick={() => handleDelete(r.id)}
                                            disabled={deleting === r.id || r.id === 'default'}
                                            title={r.id === 'default' ? 'Cannot delete the default region' : 'Delete'}
                                            className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost) disabled:opacity-40 disabled:cursor-not-allowed"
                                        >
                                            {deleting === r.id ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                                        </button>
                                    </div>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            {/* Create / Edit Modal */}
            {modal && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
                    <ModalPanel as="form" onSubmit={handleSubmit} className="card w-full max-w-md mx-4">
                        <div className="modal-header flex items-center justify-between">
                            <h3 className="modal-title">{modal.mode === 'create' ? 'Add Region' : `Edit Region — ${modal.original?.id}`}</h3>
                            <button type="button" onClick={closeModal} className="text-(--base-07) hover:text-(--error-light)">
                                <X size={18} />
                            </button>
                        </div>
                        <div className="p-6 space-y-4">
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label flex items-center gap-1.5">
                                    Region ID
                                    <HelpTip label="About the region ID">
                                        <p className="mb-2">
                                            This string is a <strong>join key</strong>, not a label. Nodes and
                                            servers carry it, the gateway tags traffic with it, and the traffic
                                            breakdown is grouped by it.
                                        </p>
                                        <p className="mb-2">
                                            So it has to match exactly what the gateway side uses for the same
                                            place. A typo does not fail anywhere - it quietly produces a second
                                            region that nothing else knows about, and the traffic recorded under
                                            it belongs to no one.
                                        </p>
                                        <p>
                                            It cannot be renamed afterwards, because everything already stamped
                                            with it would keep pointing at the old string.
                                        </p>
                                    </HelpTip>
                                </label>
                                <input
                                    type="text"
                                    value={form.id}
                                    onChange={e => setForm({ ...form, id: e.target.value })}
                                    disabled={modal.mode === 'edit'}
                                    placeholder="e.g. eu, us-east"
                                    className="input-mono w-full disabled:opacity-60 disabled:cursor-not-allowed"
                                    autoFocus={modal.mode === 'create'}
                                    required
                                />
                                <p className="text-xs text-(--base-06)">Lowercase letters, digits and hyphens. 2–32 chars. Cannot be changed after creation.</p>
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Display name</label>
                                <input
                                    type="text"
                                    value={form.displayName}
                                    onChange={e => setForm({ ...form, displayName: e.target.value })}
                                    placeholder="e.g. Europe (Frankfurt)"
                                    className="input-field w-full"
                                    required
                                    autoFocus={modal.mode === 'edit'}
                                />
                            </div>
                            <div className="flex flex-col gap-[5px]">
                                <label className="input-label">Color (optional)</label>
                                <div className="flex items-center gap-2">
                                    <input
                                        type="color"
                                        value={form.color || '#7048C8'}
                                        onChange={e => setForm({ ...form, color: e.target.value })}
                                        className="w-10 h-10 rounded cursor-pointer"
                                    />
                                    <input
                                        type="text"
                                        value={form.color}
                                        onChange={e => setForm({ ...form, color: e.target.value })}
                                        placeholder="#7048C8 (blank = none)"
                                        className="input-mono flex-1"
                                    />
                                    {form.color && (
                                        <button
                                            type="button"
                                            onClick={() => setForm({ ...form, color: '' })}
                                            className="btn btn-secondary text-xs px-2"
                                        >
                                            Clear
                                        </button>
                                    )}
                                </div>
                            </div>
                            <label className="flex items-center gap-2 cursor-pointer">
                                <input
                                    type="checkbox"
                                    checked={form.enabled}
                                    onChange={e => setForm({ ...form, enabled: e.target.checked })}
                                    className="checkbox"
                                />
                                <span className="text-sm">Enabled</span>
                            </label>
                            {error && <p className="text-(--error-light) text-sm">{error}</p>}
                        </div>
                        <div className="modal-footer flex gap-2 justify-end">
                            <button type="button" onClick={closeModal} className="btn btn-secondary">Cancel</button>
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
                    {toast.msg}
                </div>
            )}
        </SettingsPage>
    );
}
