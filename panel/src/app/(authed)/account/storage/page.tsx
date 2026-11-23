"use client";

import Checkbox from '@/components/ui/Checkbox';
import Select from '@/components/ui/Select';
import React, { useCallback, useEffect, useState } from 'react';
import { HardDrive, Plus, Trash2, Pencil, X, ShieldCheck, AlertTriangle } from 'lucide-react';
import PageHeader from '@/components/ui/PageHeader';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import Callout from '@/components/server/Callout';
import {
    listOwnBackupStorages, createOwnBackupStorage, updateOwnBackupStorage,
    deleteOwnBackupStorage, testOwnBackupStorage, ownStorageIncomplete,
    getStoragePrefs, putStoragePrefs, storagePrefPayload,
    type OwnBackupStorage, type OwnStorageInput, type StoragePrefs, type StorageFeature,
} from '@/lib/api/ownBackupStorage';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';
import StorageUploadCap from '@/components/settings/StorageUploadCap';
import { useAppData } from '@/lib/AppDataContext';
import { templatesVisible } from '@/lib/templates';

// A tenant's own S3 targets, and which one backups and templates go to. Under
// /account/ because the storage belongs to the user rather than to the
// platform: it is theirs across every server they own, and nobody else can see
// or target it.
//
// The sentences that matter to the reader are on the page, not just here: what
// lands in their bucket has no size limit from us and is outside their bill, we
// never delete from it, and switching only affects new data. Each is a
// consequence they would otherwise have to infer from a billing page. Bill,
// allowance and "Dylaris storage" wording is for the SaaS (features.hosted)
// only; a self-hosted platform has no bill and its own name for the storage.

const emptyForm = (): OwnStorageInput => ({
    name: '',
    uploadCapMbit: null,
    config: { endpoint: '', region: '', bucket: '', prefix: '', accessKeyId: '', secretAccessKey: '', forcePathStyle: false },
});

export default function StoragePage() {
    const { featureFlags, user } = useAppData();
    const hosted = !!featureFlags.hosted;
    // An older Core does not send it, and own storage was always open then.
    const ownOff = featureFlags.ownStorage === false;
    const showTemplates = templatesVisible(featureFlags.templates, !!user?.isAdmin);
    const platformName = hosted ? 'Dylaris storage' : 'platform storage';
    const [storages, setStorages] = useState<OwnBackupStorage[]>([]);
    const [loading, setLoading] = useState(true);
    const [listError, setListError] = useState<string | null>(null);
    const [noCapability, setNoCapability] = useState(false);
    const [editing, setEditing] = useState<OwnBackupStorage | null>(null);
    const [form, setForm] = useState<OwnStorageInput | null>(null);
    const [confirmDelete, setConfirmDelete] = useState<OwnBackupStorage | null>(null);
    const [saving, runSave] = useBusy();
    const [deleting, runDelete] = useBusy();
    const [testingId, setTestingId] = useState<number | null>(null);
    const [prefs, setPrefs] = useState<StoragePrefs | null>(null);
    const [prefsError, setPrefsError] = useState<string | null>(null);
    const [savingPref, setSavingPref] = useState<StorageFeature | null>(null);

    const load = useCallback(async () => {
        const [res, pr] = await Promise.all([listOwnBackupStorages(), getStoragePrefs()]);
        // Saying so beats an empty list, which reads as "you have none" and
        // invites the user to try adding one.
        setListError(res.success ? null : res.message || 'Could not load your storage.');
        // Listing stays open while own storage is off, so a 403 here is always
        // a missing capability, never own_storage_disabled. Retrying cannot help.
        setNoCapability(!res.success && res.status === 403);
        setStorages(res.storages || []);
        if (pr.success) {
            setPrefs({ backupsStorageId: pr.backupsStorageId ?? null, templatesStorageId: pr.templatesStorageId ?? null });
            setPrefsError(null);
        } else {
            setPrefsError(pr.message || 'Could not load where your data goes.');
        }
        setLoading(false);
    }, []);

    const choose = async (feature: StorageFeature, value: string) => {
        setSavingPref(feature);
        const res = await putStoragePrefs(storagePrefPayload(feature, value));
        setSavingPref(null);
        if (!res.success) { toast(res.message || 'Could not save your choice', false); return; }
        toast(feature === 'backups' ? 'New backups will go there.' : 'New templates will go there.');
        load();
    };

    useEffect(() => { load(); }, [load]);

    const startCreate = () => { setEditing(null); setForm(emptyForm()); };

    const startEdit = (s: OwnBackupStorage) => {
        setEditing(s);
        setForm({
            name: s.name,
            uploadCapMbit: s.uploadCapMbit ?? null,
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
        <main className="flex-1 overflow-y-auto app-page">
            <div className="space-y-6">
                <PageHeader
                    icon={<HardDrive size={16} />}
                    title="Storage"
                    description={ownOff ? 'Your own storage targets.' : <>
                        Connect an S3-compatible bucket of your own, then choose whether your backups
                        {showTemplates ? ' and templates' : ''} go there or to {platformName}.
                    </>}
                    actions={!listError && !ownOff ? (
                        <button onClick={startCreate} className="btn btn-primary btn-sm">
                            <Plus size={14} /> Connect storage
                        </button>
                    ) : undefined}
                />

                {ownOff && (
                    <Callout tone="warning" icon={<AlertTriangle size={16} />} role="status">
                        Your administrator has turned off own storage. Existing data stays reachable.
                        New backups{showTemplates ? ' and templates' : ''} go to {platformName}.
                    </Callout>
                )}

                {!listError && !ownOff && (
                    <Callout tone="info" icon={<ShieldCheck size={16} />}>
                        {hosted
                            ? 'Your own targets have no size limit from us and are never billed to you here - you are already paying whoever hosts them. We also never clean them up: if your account is suspended, what is in your bucket stays exactly where it is.'
                            : 'Your own targets have no size limit from the platform, and it never cleans them up: what is in your bucket stays exactly where it is.'}
                    </Callout>
                )}

                <Section title="Targets">
                    {noCapability ? (
                        <EmptyState
                            icon={<AlertTriangle size={20} />}
                            title="No access to storage targets"
                            description="Your role does not include managing storage targets. Ask an administrator."
                        />
                    ) : listError ? (
                        <EmptyState
                            tone="error"
                            icon={<AlertTriangle size={20} />}
                            title="Could not load your storage"
                            description={listError}
                            action={<button onClick={() => { setLoading(true); load(); }} className="btn btn-secondary btn-sm">Retry</button>}
                        />
                    ) : loading ? (
                        <SkeletonList rows={2} />
                    ) : storages.length === 0 ? (
                        <EmptyState
                            icon={<HardDrive size={20} />}
                            title="No storage connected"
                            description={<>
                                Your backups{showTemplates ? ' and templates' : ''} go to {platformName}
                                {hosted ? ', against your included allowance' : ''}.
                            </>}
                            action={ownOff ? undefined : <button onClick={startCreate} className="btn btn-primary btn-sm"><Plus size={14} /> Connect storage</button>}
                        />
                    ) : (
                        <div className="srv-surface">
                            <RowList label="Storage targets">
                                {storages.map(s => (
                                    <Row
                                        key={s.id}
                                        leading={<HardDrive size={16} />}
                                        title={
                                            <span className="flex flex-wrap items-center gap-2">
                                                <span className="truncate">{s.name}</span>
                                                {prefs?.backupsStorageId === s.id && <span className="badge badge-accent">Backups</span>}
                                                {prefs?.templatesStorageId === s.id && <span className="badge badge-accent">Templates</span>}
                                            </span>
                                        }
                                        meta={
                                            <span className="font-mono">
                                                {s.config?.bucket}{s.config?.prefix ? `/${s.config.prefix}` : ''}
                                                <span className="text-(--base-05)"> at </span>
                                                {s.config?.endpoint}
                                            </span>
                                        }
                                        actions={
                                            <>
                                                <button
                                                    onClick={() => runTest(s)}
                                                    disabled={testingId === s.id}
                                                    className="btn btn-secondary btn-sm disabled:opacity-50"
                                                >
                                                    {testingId === s.id ? 'Testing...' : 'Test'}
                                                </button>
                                                <button onClick={() => startEdit(s)} className="btn btn-icon btn-sm" aria-label={`Edit ${s.name}`}>
                                                    <Pencil size={14} />
                                                </button>
                                                <button
                                                    onClick={() => setConfirmDelete(s)}
                                                    className="btn btn-icon btn-sm text-(--error-light)"
                                                    aria-label={`Remove ${s.name}`}
                                                >
                                                    <Trash2 size={14} />
                                                </button>
                                            </>
                                        }
                                    />
                                ))}
                            </RowList>
                        </div>
                    )}
                </Section>

                {!listError && !ownOff && !loading && (
                    <Section
                        title="Where new data goes"
                        description={<>
                            Switching affects new backups{showTemplates ? ' and templates' : ''} only. What
                            already exists stays where it was written.
                        </>}
                    >
                        {prefsError ? (
                            <EmptyState
                                tone="error"
                                icon={<AlertTriangle size={20} />}
                                title="Could not load where your data goes"
                                description={prefsError}
                                action={<button onClick={() => { setLoading(true); load(); }} className="btn btn-secondary btn-sm">Retry</button>}
                            />
                        ) : prefs && (
                            <div className="srv-surface p-5 grid grid-cols-1 sm:grid-cols-2 gap-4">
                                <PrefSelect
                                    id="pref-backups"
                                    label="Backups go to"
                                    value={prefs.backupsStorageId}
                                    storages={storages}
                                    platformLabel={platformName}
                                    saving={savingPref === 'backups'}
                                    disabled={savingPref !== null}
                                    onChange={v => choose('backups', v)}
                                    hint="Used by every backup job that does not name a storage itself."
                                />
                                {showTemplates && (
                                    <PrefSelect
                                        id="pref-templates"
                                        label="Templates go to"
                                        value={prefs.templatesStorageId}
                                        storages={storages}
                                        platformLabel={platformName}
                                        saving={savingPref === 'templates'}
                                        disabled={savingPref !== null}
                                        onChange={v => choose('templates', v)}
                                        hint="On your own target the template size and count limits do not apply."
                                    />
                                )}
                            </div>
                        )}
                    </Section>
                )}
            </div>

            {form && (
                <StorageDialog
                    form={form}
                    setForm={setForm}
                    isEdit={!!editing}
                    storageId={editing?.id}
                    saving={saving}
                    onCancel={() => { setForm(null); setEditing(null); }}
                    onSave={save}
                />
            )}

            {confirmDelete && (
                <div className="modal-overlay animate-fade-in">
                    <ModalPanel className="modal-panel max-w-md" role="alertdialog">
                        <h2 className="text-base font-medium text-(--base-09)">
                            Remove {confirmDelete.name}?
                        </h2>
                        <p className="text-sm text-(--base-07) mt-2 leading-relaxed">
                            This removes our record of how to reach the bucket. It only works once nothing
                            here uses it: choose another storage for backups and templates on this page, move
                            any backup schedule to another storage, and delete the backups and templates
                            stored in it first.
                        </p>
                        <div className="flex justify-end gap-2 mt-5">
                            <button onClick={() => setConfirmDelete(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={remove} disabled={deleting} className="btn btn-danger disabled:opacity-50">
                                {deleting ? 'Removing...' : 'Remove'}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </main>
    );
}

function StorageDialog({
    form, setForm, isEdit, storageId, saving, onCancel, onSave,
}: {
    form: OwnStorageInput;
    setForm: (f: OwnStorageInput) => void;
    isEdit: boolean;
    storageId?: number;
    saving: boolean;
    onCancel: () => void;
    onSave: () => void;
}) {
    const set = (patch: Partial<OwnStorageInput>) => setForm({ ...form, ...patch });
    const setCfg = (patch: Partial<OwnStorageInput['config']>) => setForm({ ...form, config: { ...form.config, ...patch } });

    return (
        <div className="modal-overlay animate-fade-in">
            <ModalPanel className="modal-panel max-w-lg">
                <div className="flex items-start justify-between gap-3 mb-4">
                    <h2 className="text-base font-medium text-(--base-09)">
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

                    <Checkbox
                        className="pt-1"
                        checked={form.config.forcePathStyle || false}
                        onChange={v => setCfg({ forcePathStyle: v })}
                        label="Path-style addressing"
                        hint="Needed by MinIO and some self-hosted gateways."
                    />

                    <div className="border-t border-(--base-04) pt-3">
                        <StorageUploadCap
                            value={form.uploadCapMbit}
                            onChange={uploadCapMbit => set({ uploadCapMbit })}
                            storageId={storageId}
                            mine
                        />
                    </div>

                    {!isEdit && (
                        <p className="text-xs text-(--base-06)">
                            Connecting changes nothing yet: once it is connected, choose it for backups or
                            templates on this page.
                        </p>
                    )}
                </div>

                <div className="flex justify-end gap-2 mt-5">
                    <button onClick={onCancel} className="btn btn-secondary">Cancel</button>
                    <button onClick={onSave} disabled={saving} className="btn btn-primary disabled:opacity-50">
                        {saving ? 'Saving...' : isEdit ? 'Save' : 'Connect'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

function PrefSelect({
    id, label, value, storages, platformLabel, saving, disabled, onChange, hint,
}: {
    id: string;
    label: string;
    value: number | null;
    storages: OwnBackupStorage[];
    platformLabel: string;
    saving: boolean;
    disabled: boolean;
    onChange: (value: string) => void;
    hint: string;
}) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="srv-label" htmlFor={id}>
                {label}{saving && <span className="text-(--base-06)"> (saving...)</span>}
            </label>
            <Select
                id={id}
                value={value == null ? '' : String(value)}
                disabled={disabled}
                onChange={onChange}
                options={[
                    { value: '', label: platformLabel.charAt(0).toUpperCase() + platformLabel.slice(1) },
                    ...storages.map(s => ({ value: String(s.id), label: s.name })),
                ]}
            />
            <p className="text-xs text-(--base-06)">{hint}</p>
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
