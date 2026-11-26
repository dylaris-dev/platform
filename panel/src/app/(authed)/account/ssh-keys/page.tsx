"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { KeyRound, Plus, Trash2, AlertTriangle, Shield, X } from 'lucide-react';
import PageHeader from '@/components/ui/PageHeader';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import Callout from '@/components/server/Callout';
import { useAppData } from '@/lib/AppDataContext';
import { listSSHKeys, addSSHKey, deleteSSHKey, type SSHKey } from '@/lib/api/sshKeys';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDateTime } from '@/lib/time';

// per-user SSH keys for SFTP. Modelled on the API keys page beside it: same
// reauth on add, same confirm dialog on delete.

export default function SshKeysPage() {
    const { user } = useAppData();
    const [keys, setKeys] = useState<SSHKey[]>([]);
    const [loading, setLoading] = useState(true);
    const [loadError, setLoadError] = useState<string | null>(null);
    const [adding, setAdding] = useState(false);
    const [form, setForm] = useState({ name: '', publicKey: '' });

    // Kept out of `form` for the same reason as on the API keys page: the
    // password must not outlive the dialog it was typed into.
    const [reauthPassword, setReauthPassword] = useState('');
    const [reauthCode, setReauthCode] = useState('');

    const [deleting, setDeleting] = useState<SSHKey | null>(null);
    const [addingKey, runAdd] = useBusy();
    const [deletingKey, runDelete] = useBusy();

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const res = await listSSHKeys();
        if (res.success && res.keys) {
            setKeys(res.keys);
            setLoadError(null);
        } else if (!res.success) {
            setLoadError(res.message || 'Could not load SSH keys.');
        }
        setLoading(false);
    }, []);

    useEffect(() => { refresh(); }, [refresh]);

    const closeDialog = () => {
        setAdding(false);
        setReauthPassword('');
        setReauthCode('');
    };

    const handleAdd = async () => {
        const name = form.name.trim();
        const publicKey = form.publicKey.trim();
        if (!name) { showToast('Name required', false); return; }
        if (!publicKey) { showToast('Public key required', false); return; }
        const res = await addSSHKey({
            name,
            publicKey,
            password: reauthPassword,
            code: reauthCode.replace(/\s/g, '') || undefined,
        });
        if (res.success) {
            closeDialog();
            setForm({ name: '', publicKey: '' });
            showToast(res.note || 'SSH key added.', true);
            refresh();
        } else {
            showToast(res.message || 'Add failed', false);
        }
    };

    const handleDelete = async () => {
        if (!deleting) return;
        const res = await deleteSSHKey(deleting.id);
        setDeleting(null);
        if (res.success) {
            showToast('SSH key removed.', true);
            refresh();
        } else {
            showToast(res.message || 'Remove failed', false);
        }
    };

    if (!user) return null;

    return (
        <main className="flex-1 overflow-y-auto app-page space-y-6">
            <PageHeader
                icon={<KeyRound size={16} />}
                title="SSH Keys"
                description="Public keys that sign you in to SFTP."
                actions={
                    <button onClick={() => setAdding(true)} className="btn btn-primary btn-sm">
                        <Plus size={14} />
                        Add SSH key
                    </button>
                }
            />

            <Callout tone="info" icon={<Shield size={16} />}>
                SSH keys sign you in to SFTP. Accounts with two-factor authentication must use a key;
                the password alone is not accepted over SFTP.
            </Callout>

            <Section title="Your keys">
                {loading ? (
                    <SkeletonList rows={3} />
                ) : loadError ? (
                    <EmptyState
                        tone="error"
                        icon={<AlertTriangle size={20} />}
                        title="Could not load SSH keys"
                        description={loadError}
                        action={<button onClick={() => { setLoading(true); refresh(); }} className="btn btn-secondary btn-sm">Retry</button>}
                    />
                ) : keys.length === 0 ? (
                    <EmptyState
                        icon={<KeyRound size={20} />}
                        title="No SSH keys yet"
                        description="Add a public key to sign in to SFTP without a password."
                        action={<button onClick={() => setAdding(true)} className="btn btn-primary btn-sm"><Plus size={14} />Add SSH key</button>}
                    />
                ) : (
                    <div className="srv-surface">
                        <RowList label="SSH keys">
                            {keys.map(k => (
                                <Row
                                    key={k.id}
                                    leading={<KeyRound size={16} className="text-(--accent-light)" />}
                                    title={k.name}
                                    meta={
                                        <span className="block space-y-0.5 whitespace-normal">
                                            <span className="block font-mono break-all">{k.fingerprint}</span>
                                            <span className="block">Added: <span className="text-(--base-08)">{fmtDateTime(k.createdAt)}</span></span>
                                        </span>
                                    }
                                    actions={
                                        <button onClick={() => setDeleting(k)} aria-label={`Remove ${k.name}`} className="btn btn-secondary btn-sm shrink-0">
                                            <Trash2 size={14} className="text-(--error-light)" />
                                            Remove
                                        </button>
                                    }
                                />
                            ))}
                        </RowList>
                    </div>
                )}
            </Section>

            {/* Add modal */}
            {adding && (
                <div className="modal-overlay animate-fade-in" onClick={closeDialog}>
                    <ModalPanel onClose={closeDialog} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header flex items-start justify-between gap-3">
                            <h3 className="modal-title flex items-center gap-2">
                                <KeyRound size={16} />
                                Add SSH key
                            </h3>
                            <button
                                onClick={closeDialog}
                                aria-label="Close"
                                className="shrink-0 p-1 rounded text-(--base-06) hover:bg-(--base-03) hover:text-(--base-08) transition-colors"
                            >
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div>
                                <label htmlFor="sshkey-name" className="input-label">Name</label>
                                <input
                                    id="sshkey-name"
                                    type="text"
                                    value={form.name}
                                    onChange={e => setForm({ ...form, name: e.target.value })}
                                    className="input-field w-full"
                                    placeholder="laptop"
                                    maxLength={128}
                                />
                            </div>
                            <div>
                                <label htmlFor="sshkey-public" className="input-label">Public key</label>
                                <textarea
                                    id="sshkey-public"
                                    value={form.publicKey}
                                    onChange={e => setForm({ ...form, publicKey: e.target.value })}
                                    className="input-field input-mono w-full h-28 resize-none break-all"
                                    placeholder="ssh-ed25519 AAAA... you@laptop"
                                    spellCheck={false}
                                />
                                <p className="text-xs text-(--base-06) mt-1">
                                    Paste the contents of your .pub file. Create one with:{' '}
                                    <code className="font-mono">ssh-keygen -t ed25519</code>
                                </p>
                            </div>
                        </div>
                        <ReauthFields
                            idPrefix="sshkey"
                            twoFactorEnabled={!!user?.is2FAEnabled}
                            password={reauthPassword}
                            code={reauthCode}
                            onPassword={setReauthPassword}
                            onCode={setReauthCode}
                            disabled={addingKey}
                        />
                        <div className="modal-footer">
                            <button onClick={closeDialog} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runAdd(handleAdd)} disabled={addingKey || !reauthReady(!!user?.is2FAEnabled, reauthPassword, reauthCode)} className="btn btn-primary disabled:opacity-40">Add key</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Remove confirmation */}
            {deleting && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeleting(null)}>
                    <ModalPanel onClose={() => setDeleting(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                Remove {deleting.name}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                SFTP sign-ins with this key stop working immediately.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeleting(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingKey} className="btn btn-danger disabled:opacity-40">Remove</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

        </main>
    );
}
