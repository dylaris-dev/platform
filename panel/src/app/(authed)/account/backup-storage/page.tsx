"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { HardDrive, Plus, Trash2, Star, Pencil, X, ShieldCheck, AlertTriangle } from 'lucide-react';
import {
    listOwnBackupStorages, createOwnBackupStorage, updateOwnBackupStorage,
    deleteOwnBackupStorage, testOwnBackupStorage, ownStorageIncomplete,
    type OwnBackupStorage, type OwnStorageInput,
} from '@/lib/api/ownBackupStorage';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';

// A tenant's own S3 for backups. Under /account/ because the storage belongs to
// the user rather than to the platform: it is theirs across every server they
// own, and nobody else can see or target it.
//
// The two sentences that matter to the reader are on the page, not just here:
// what lands in their bucket is outside our quota and outside their bill, and we
// never delete from it. Both are consequences they would otherwise have to infer
// from a billing page.

const emptyForm = (): OwnStorageInput => ({
    name: '',
    isDefault: false,
    config: { endpoint: '', region: '', bucket: '', prefix: '', accessKeyId: '', secretAccessKey: '', forcePathStyle: false },
});

export default function OwnBackupStoragePage() {
    const [storages, setStorages] = useState<OwnBackupStorage[]>([]);
    const [loading, setLoading] = useState(true);
    const [denied, setDenied] = useState(false);
    const [editing, setEditing] = useState<OwnBackupStorage | null>(null);
    const [form, setForm] = useState<OwnStorageInput | null>(null);
    const [confirmDelete, setConfirmDelete] = useState<OwnBackupStorage | null>(null);
    const [saving, runSave] = useBusy();
    const [deleting, runDelete] = useBusy();
    const [testingId, setTestingId] = useState<number | null>(null);

    const load = useCallback(async () => {
        const res = await listOwnBackupStorages();
        // A capability-less account gets 403 here. Saying so beats an empty list,
        // which reads as "you have none" and invites the user to try adding one.
        if (!res.success) setDenied(true);
        setStorages(res.storages || []);
        setLoading(false);
    }, []);

    useEffect(() => { load(); }, [load]);

    const startCreate = () => { setEditing(null); setForm(emptyForm()); };

    const startEdit = (s: OwnBackupStorage) => {
        setEditing(s);
        setForm({
            name: s.name,
            isDefault: s.isDefault,
            // The secret is deliberately blank: Core never returns it, and an
            // empty field on save means "keep the stored one".
            config: { ...s.config, secretAccessKey: '' },
        });
    };

    const save = () => {
        if (!form) return;
        const problem = ownStorageIncomplete(form, !!editing);
        if (problem) { toast(problem, false); return; }
        runSave(async () => {
            const res = editing
                ? await updateOwnBackupStorage(editing.id, form)
                : await createOwnBackupStorage(form);
            if (!res.success) { toast(res.message || 'Could not save this storage', false); return; }
            toast(editing ? 'Storage updated.' : 'Storage connected.');
            setForm(null);
            setEditing(null);
            load();
        });
    };

    const runTest = async (s: OwnBackupStorage) => {
        setTestingId(s.id);
        const res = await testOwnBackupStorage(s.id);
        setTestingId(null);
        toast(res.success ? `${s.name} is reachable and writable.` : res.message || 'The test failed.', res.success);
    };

    const remove = () => {
        if (!confirmDelete) return;
        runDelete(async () => {
            const res = await deleteOwnBackupStorage(confirmDelete.id);
            if (!res.success) { toast(res.message || 'Could not remove this storage', false); return; }
            toast('Storage removed.');
            setConfirmDelete(null);
            load();
        });
    };

    return (
        <main className="flex-1 overflow-y-auto p-6">
            <div className="max-w-3xl mx-auto space-y-5">
                <header className="flex flex-wrap items-start justify-between gap-3">
                    <div className="flex items-start gap-3 min-w-0">
                        <HardDrive size={20} className="text-(--base-06) mt-1 shrink-0" />
                        <div className="min-w-0">
                            <h1 className="text-lg font-medium text-(--base-09)">Your backup storage</h1>
                            <p className="text-sm text-(--base-06) mt-1 max-w-xl">
                                Connect an S3-compatible bucket of your own and your backups go there instead
                                of ours.
                            </p>
                        </div>
                    </div>
                    {!denied && (
                        <button onClick={startCreate} className="btn btn-primary flex items-center gap-2 shrink-0">
                            <Plus size={16} /> Connect storage
                        </button>
                    )}
                </header>

                {!denied && (
                    <div className="card p-4 flex items-start gap-2.5">
                        <ShieldCheck size={16} className="text-(--base-06) mt-0.5 shrink-0" />
                        <p className="text-sm text-(--base-07) leading-relaxed">
                            What lands in your own bucket does not count against your backup allowance and is
                            never billed to you here - you are already paying whoever hosts it. It also means
                            we never delete from it on our own: if your account is suspended, those archives
                            stay exactly where they are.
                        </p>
                    </div>
                )}

                {denied ? (
                    <div className="card p-5 flex items-start gap-2.5">
                        <AlertTriangle size={16} className="text-(--warning) mt-0.5 shrink-0" />
                        <p className="text-sm text-(--base-07)">
                            Your account is not allowed to connect its own backup storage. An administrator
                            grants this per role.
                        </p>
                    </div>
                ) : loading ? (
                    <SkeletonList rows={2} />
                ) : storages.length === 0 ? (
                    <div className="card p-8 text-center">
                        <p className="text-sm text-(--base-07)">No storage connected.</p>
                        <p className="text-xs text-(--base-06) mt-1.5 max-w-md mx-auto">
                            Your backups currently go to the storage the platform provides, against your
                            included allowance.
                        </p>
                    </div>
                ) : (
                    <ul className="space-y-3">
                        {storages.map(s => (
                            <li key={s.id} className="card p-4">
                                <div className="flex flex-wrap items-start justify-between gap-3">
                                    <div className="min-w-0">
                                        <div className="flex items-center gap-2 flex-wrap">
                                            <span className="text-sm font-medium text-(--base-09) truncate">{s.name}</span>
                                            {s.isDefault && (
                                                <span className="badge badge-accent flex items-center gap-1">
                                                    <Star size={11} /> Default
                                                </span>
                                            )}
                                        </div>
                                        <p className="text-xs text-(--base-06) mt-1 font-mono break-all">
                                            {s.config?.bucket}{s.config?.prefix ? `/${s.config.prefix}` : ''}
                                            <span className="text-(--base-05)"> at </span>
                                            {s.config?.endpoint}
                                        </p>
                                    </div>
                                    <div className="flex items-center gap-2 shrink-0">
                                        <button
                                            onClick={() => runTest(s)}
                                            disabled={testingId === s.id}
                                            className="btn btn-secondary text-xs disabled:opacity-50"
                                        >
                                            {testingId === s.id ? 'Testing...' : 'Test'}
                                        </button>
                                        <button onClick={() => startEdit(s)} className="btn btn-icon btn-sm" aria-label={`Edit ${s.name}`}>
                                            <Pencil size={15} />
                                        </button>
                                        <button
                                            onClick={() => setConfirmDelete(s)}
                                            className="btn btn-icon btn-sm text-(--error-light)"
                                            aria-label={`Remove ${s.name}`}
                                        >
                                            <Trash2 size={15} />
                                        </button>
                                    </div>
                                </div>
                            </li>
                        ))}
                    </ul>
                )}
            </div>

            {form && (
                <StorageDialog
                    form={form}
                    setForm={setForm}
                    isEdit={!!editing}
                    saving={saving}
                    onCancel={() => { setForm(null); setEditing(null); }}
                    onSave={save}
                />
            )}

            {confirmDelete && (
                <div className="modal-overlay animate-fade-in" role="dialog" aria-modal="true" aria-labelledby="remove-storage-title">
                    <div className="modal-panel max-w-md">
                        <h2 id="remove-storage-title" className="text-base font-medium text-(--base-09)">
                            Remove {confirmDelete.name}?
                        </h2>
                        <p className="text-sm text-(--base-07) mt-2 leading-relaxed">
                            This removes our record of how to reach the bucket. It only works once nothing
                            here uses it: move any backup schedule to another storage, and delete the backups
                            stored in it first.
                        </p>
                        <div className="flex justify-end gap-2 mt-5">
                            <button onClick={() => setConfirmDelete(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={remove} disabled={deleting} className="btn btn-danger disabled:opacity-50">
                                {deleting ? 'Removing...' : 'Remove'}
                            </button>
                        </div>
                    </div>
                </div>
            )}
        </main>
    );
}

function StorageDialog({
    form, setForm, isEdit, saving, onCancel, onSave,
}: {
    form: OwnStorageInput;
    setForm: (f: OwnStorageInput) => void;
    isEdit: boolean;
    saving: boolean;
    onCancel: () => void;
    onSave: () => void;
}) {
    const set = (patch: Partial<OwnStorageInput>) => setForm({ ...form, ...patch });
    const setCfg = (patch: Partial<OwnStorageInput['config']>) => setForm({ ...form, config: { ...form.config, ...patch } });

    return (
        <div className="modal-overlay animate-fade-in" role="dialog" aria-modal="true" aria-labelledby="storage-dialog-title">
            <div className="modal-panel max-w-lg">
                <div className="flex items-start justify-between gap-3 mb-4">
                    <h2 id="storage-dialog-title" className="text-base font-medium text-(--base-09)">
                        {isEdit ? 'Edit storage' : 'Connect S3 storage'}
                    </h2>
                    <button onClick={onCancel} className="btn btn-icon btn-sm" aria-label="Close"><X size={16} /></button>
                </div>

                <div className="space-y-3">
                    <Field label="Name" id="st-name" hint="Yours to recognise it by.">
                        <input id="st-name" className="input-field w-full" value={form.name}
                            onChange={e => set({ name: e.target.value })} placeholder="Backblaze" />
                    </Field>
                    <Field label="Endpoint" id="st-endpoint" hint="The full https URL of the S3 API.">
                        <input id="st-endpoint" className="input-field w-full" value={form.config.endpoint || ''}
                            onChange={e => setCfg({ endpoint: e.target.value })}
                            placeholder="https://s3.eu-central-003.backblazeb2.com" />
                    </Field>
                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <Field label="Bucket" id="st-bucket">
                            <input id="st-bucket" className="input-field w-full" value={form.config.bucket || ''}
                                onChange={e => setCfg({ bucket: e.target.value })} />
                        </Field>
                        <Field label="Region" id="st-region" hint="Optional.">
                            <input id="st-region" className="input-field w-full" value={form.config.region || ''}
                                onChange={e => setCfg({ region: e.target.value })} placeholder="auto" />
                        </Field>
                    </div>
                    <Field label="Prefix" id="st-prefix" hint="Optional. A folder inside the bucket.">
                        <input id="st-prefix" className="input-field w-full" value={form.config.prefix || ''}
                            onChange={e => setCfg({ prefix: e.target.value })} placeholder="dylaris/" />
                    </Field>
                    <Field label="Access key ID" id="st-ak">
                        <input id="st-ak" className="input-field w-full" value={form.config.accessKeyId || ''}
                            onChange={e => setCfg({ accessKeyId: e.target.value })} autoComplete="off" />
                    </Field>
                    <Field
                        label="Secret access key"
                        id="st-sk"
                        hint={isEdit
                            ? 'Leave blank to keep the stored one. Required if you change the endpoint, bucket or access key.'
                            : 'Stored encrypted and never shown again.'}
                    >
                        <input id="st-sk" type="password" className="input-field w-full"
                            value={form.config.secretAccessKey || ''}
                            onChange={e => setCfg({ secretAccessKey: e.target.value })}
                            autoComplete="new-password"
                            placeholder={isEdit ? 'unchanged' : ''} />
                    </Field>

                    <label className="flex items-start gap-2.5 cursor-pointer pt-1">
                        <input type="checkbox" className="mt-0.5" checked={form.config.forcePathStyle || false}
                            onChange={e => setCfg({ forcePathStyle: e.target.checked })} />
                        <span className="min-w-0">
                            <span className="block text-sm text-(--base-08)">Path-style addressing</span>
                            <span className="block text-xs text-(--base-06) mt-0.5">
                                Needed by MinIO and some self-hosted gateways.
                            </span>
                        </span>
                    </label>

                    <label className="flex items-start gap-2.5 cursor-pointer">
                        <input type="checkbox" className="mt-0.5" checked={form.isDefault}
                            onChange={e => set({ isDefault: e.target.checked })} />
                        <span className="min-w-0">
                            <span className="block text-sm text-(--base-08)">Use this for my backups by default</span>
                            <span className="block text-xs text-(--base-06) mt-0.5">
                                Every backup job that has not been pointed somewhere specific will write here.
                                Without this, connecting a storage changes nothing until you pick it on a job.
                            </span>
                        </span>
                    </label>
                </div>

                <div className="flex justify-end gap-2 mt-5">
                    <button onClick={onCancel} className="btn btn-secondary">Cancel</button>
                    <button onClick={onSave} disabled={saving} className="btn btn-primary disabled:opacity-50">
                        {saving ? 'Saving...' : isEdit ? 'Save' : 'Connect'}
                    </button>
                </div>
            </div>
        </div>
    );
}

function Field({ label, id, hint, children }: { label: string; id: string; hint?: string; children: React.ReactNode }) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label" htmlFor={id}>{label}</label>
            {children}
            {hint && <p className="text-xs text-(--base-06)">{hint}</p>}
        </div>
    );
}
