"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { KeyRound, Plus, Trash2, AlertTriangle, Shield, X } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listSSHKeys, addSSHKey, deleteSSHKey, type SSHKey } from '@/lib/api/sshKeys';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';

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
        <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
            <header className="flex items-center gap-3 mb-4">
                <KeyRound size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">SSH Keys</h1>
                <div className="ml-auto">
                    <button onClick={() => setAdding(true)} className="btn btn-primary btn-sm">
                        <Plus size={12} />
                        Add SSH key
                    </button>
                </div>
            </header>

            <div className="card p-4 mb-4 text-xs text-(--base-07) flex items-start gap-2">
                <Shield size={14} className="text-(--accent-light) shrink-0 mt-0.5" />
                <p>
                    SSH keys sign you in to SFTP. Accounts with two-factor authentication must use a key;
                    the password alone is not accepted over SFTP.
                </p>
            </div>

            {loading ? (
                <SkeletonList rows={3} />
            ) : loadError ? (
                <div className="card p-4 text-xs flex items-start gap-2">
                    <AlertTriangle size={14} className="text-(--error-light) shrink-0 mt-0.5" />
                    <p className="flex-1 text-(--base-08)">{loadError}</p>
                    <button onClick={() => { setLoading(true); refresh(); }} className="btn btn-secondary btn-sm shrink-0">Retry</button>
                </div>
            ) : keys.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <KeyRound size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No SSH keys yet.</p>
                </div>
            ) : (
                <div className="space-y-2">
                    {keys.map(k => (
                        <article key={k.id} className="card p-3 flex items-start gap-3">
                            <KeyRound size={16} className="text-(--accent-light)" />
                            <div className="min-w-0 flex-1">
                                <div className="font-medium text-sm text-(--base-09)">{k.name}</div>
                                <div className="mt-1 text-xs text-(--base-06) font-mono break-all">{k.fingerprint}</div>
                                <div className="text-xs text-(--base-06) mt-0.5">
                                    Added: <span className="text-(--base-08)">{new Date(k.createdAt).toLocaleString()}</span>
                                </div>
                            </div>
                            <button onClick={() => setDeleting(k)} className="btn btn-secondary btn-sm shrink-0">
                                <Trash2 size={12} className="text-(--error)" />
                                Remove
                            </button>
                        </article>
                    ))}
                </div>
            )}

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
