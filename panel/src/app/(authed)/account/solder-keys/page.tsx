"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { KeyRound, Plus, Copy, Trash2, AlertTriangle, Shield, X, EyeOff, Link2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import {
    listKeys, createKey, deleteKey, getSolderHandle, setSolderHandle,
    type SolderKey,
} from '@/lib/api/solderAccess';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';

// Per-user Solder key management. A key (?k=) grants a launcher read access
// to ALL of the owner's private packs. Treat like a password — shown once on
// creation; subsequent listings carry no key material (only the hash is stored).

export default function SolderKeysPage() {
    const { user, featureFlags } = useAppData();
    // The list endpoint is gated by RequireModpacksEnabled, so with modpacks off
    // it answers 503 and the catch below turns that into an empty list. Without
    // this flag the page would invite the user to create a key the API is
    // already refusing - which is what it used to do.
    const modpacksDisabled = !featureFlags.modpacks;
    const [keys, setKeys] = useState<SolderKey[]>([]);
    const [loading, setLoading] = useState(true);
    const [creating, setCreating] = useState(false);
    const [name, setName] = useState('');
    // Empty means "mint one". Anything typed here is the key the Technic
    // Platform issued, which is the only key Technic will ever verify.
    const [platformKey, setPlatformKey] = useState('');
    const [revealedKey, setRevealedKey] = useState<{ plaintext: string; name: string } | null>(null);
    const [deleting, setDeleting] = useState<SolderKey | null>(null);
    // The account's own Solder address. Claimed once - the Technic Platform
    // stores the URL per modpack and a launcher keeps it inside the installed
    // pack, so a rename would break every one of them silently.
    const [handle, setHandle] = useState('');
    const [solderUrl, setSolderUrl] = useState('');
    const [handleDraft, setHandleDraft] = useState('');
    const [savingHandle, runSaveHandle] = useBusy();
    const [creatingKey, runCreate] = useBusy();
    const [deletingKey, runDelete] = useBusy();

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        try {
            const data = await listKeys();
            setKeys(data);
        } catch {
            setKeys([]);
        } finally {
            setLoading(false);
        }
    }, []);

    const refreshHandle = useCallback(async () => {
        try {
            const h = await getSolderHandle();
            setHandle(h.handle);
            setSolderUrl(h.url);
        } catch {
            // Leaves the card in its "not claimed yet" state, which is the
            // honest reading of "we could not ask".
        }
    }, []);

    useEffect(() => { refresh(); refreshHandle(); }, [refresh, refreshHandle]);

    const handleClaim = async () => {
        const res = await setSolderHandle(handleDraft.trim().toLowerCase());
        if (!res.success) {
            showToast(res.message || 'Could not set the address', false);
            return;
        }
        setHandle(res.handle || '');
        setSolderUrl(res.url || '');
        showToast('Solder address set.');
    };

    const handleCreate = async () => {
        const trimmed = name.trim();
        if (!trimmed) { showToast('Name required', false); return; }
        const pasted = platformKey.trim();
        try {
            const res = await createKey(trimmed, pasted);
            if (!res.success) {
                // Both refusals are actionable, so say which one it was.
                showToast(res.message || 'Create failed', false);
                return;
            }
            setCreating(false);
            setName('');
            setPlatformKey('');
            refresh();
            // Only a key we minted has a plaintext to show, and only once. A
            // pasted one is not echoed back: the operator has it already.
            if (res.plaintext) {
                setRevealedKey({ plaintext: res.plaintext, name: trimmed });
            } else {
                showToast('Key added. Now click Link Solder on your Technic profile.');
            }
        } catch {
            showToast('Create failed', false);
        }
    };

    const handleDelete = async () => {
        if (!deleting) return;
        const res = await deleteKey(deleting.id);
        setDeleting(null);
        if (res.success) {
            showToast('Key deleted.', true);
            refresh();
        } else {
            showToast('Delete failed', false);
        }
    };

    if (!user) return null;

    return (
        <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
            <header className="flex items-center gap-3 mb-4">
                <KeyRound size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Solder Keys</h1>
                <div className="ml-auto">
                    <button onClick={() => setCreating(true)} disabled={modpacksDisabled}
                        title={modpacksDisabled ? 'Modpacks are disabled' : undefined}
                        className="btn btn-primary btn-sm disabled:opacity-40">
                        <Plus size={12} />
                        New Key
                    </button>
                </div>
            </header>

            {modpacksDisabled && (
                <div className="card p-4 mb-4 text-xs flex items-start gap-2">
                    <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <div>
                        <p className="text-(--base-08)">Modpacks are turned off on this platform.</p>
                        <p className="mt-1 text-(--base-06)">
                            New keys cannot be created and existing ones are already being refused, because Solder serves modpacks. An admin can re-enable them under Settings &rarr; Features.
                        </p>
                    </div>
                </div>
            )}

            {/* The address comes first: without it there is nothing to enter on
                the Technic Platform, and a key on its own links nothing. */}
            <section className="card p-4 mb-4">
                <div className="flex items-start gap-2">
                    <Link2 size={14} className="text-(--accent-light) shrink-0 mt-1" />
                    <div className="min-w-0 flex-1">
                        <h2 className="text-sm font-medium text-(--base-09)">Your Solder address</h2>
                        {handle ? (
                            <>
                                <p className="text-xs text-(--base-06) mt-0.5">
                                    Enter this as the Solder URL on your Technic profile, under Edit
                                    Profile, Solder Configuration.
                                </p>
                                <div className="flex items-center gap-2 mt-2">
                                    <code className="input-field input-mono flex-1 truncate">{solderUrl || `(set the public panel URL under Settings, Modpacks first)`}</code>
                                    <button
                                        type="button"
                                        onClick={() => { navigator.clipboard.writeText(solderUrl); showToast('Address copied.'); }}
                                        disabled={!solderUrl}
                                        className="btn btn-secondary btn-sm disabled:opacity-40"
                                    >
                                        <Copy size={12} /> Copy
                                    </button>
                                </div>
                            </>
                        ) : (
                            <>
                                <p className="text-xs text-(--base-06) mt-0.5">
                                    Choose the name your Solder is reached under. Your packs live in
                                    their own namespace, so their slugs only have to be unique to
                                    you. This is set <strong className="text-(--base-08)">once</strong>:
                                    the Technic Platform stores the address inside every linked
                                    modpack, and changing it later would stop each installed copy
                                    from updating.
                                </p>
                                <div className="flex items-center gap-2 mt-2">
                                    <input
                                        type="text"
                                        value={handleDraft}
                                        onChange={e => setHandleDraft(e.target.value)}
                                        onKeyDown={e => { if (e.key === 'Enter') runSaveHandle(handleClaim); }}
                                        className="input-field input-mono w-64"
                                        placeholder="e.g. bartis"
                                        maxLength={64}
                                        spellCheck={false}
                                        autoComplete="off"
                                        disabled={modpacksDisabled}
                                    />
                                    <button
                                        type="button"
                                        onClick={() => runSaveHandle(handleClaim)}
                                        disabled={savingHandle || modpacksDisabled || handleDraft.trim().length < 3}
                                        className="btn btn-primary btn-sm disabled:opacity-40"
                                    >
                                        Set address
                                    </button>
                                </div>
                            </>
                        )}
                    </div>
                </div>
            </section>

            <div className="card p-4 mb-4 text-xs text-(--base-07) flex items-start gap-2">
                <Shield size={14} className="text-(--accent-light) shrink-0 mt-0.5" />
                <p>
                    A Solder key (<code className="font-mono">?k=</code>) grants a launcher read access to{' '}
                    <strong className="text-(--base-08)">all</strong> of your private packs. Treat it like a
                    password — it is shown only once on creation and cannot be recovered afterwards.
                </p>
            </div>

            {loading ? (
                <SkeletonList rows={3} />
            ) : keys.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <KeyRound size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No keys yet.</p>
                    <p className="text-xs text-(--base-06)">Create a key and pass it as <code className="font-mono">?k=</code> to give a launcher access to all your private packs.</p>
                </div>
            ) : (
                <div className="space-y-2">
                    {keys.map(k => (
                        <article key={k.id} className="card p-3 flex items-start gap-3">
                            <KeyRound size={16} className="text-(--accent-light) mt-0.5 shrink-0" />
                            <div className="min-w-0 flex-1">
                                <span className="font-medium text-sm text-(--base-09)">{k.name}</span>
                                <div className="text-xs text-(--base-06) mt-1">
                                    Created: <span className="text-(--base-08)">{new Date(k.createdAt).toLocaleDateString()}</span>
                                </div>
                            </div>
                            <button
                                onClick={() => setDeleting(k)}
                                className="btn btn-icon btn-ghost shrink-0"
                                title="Delete key"
                            >
                                <Trash2 size={14} className="text-(--error)" />
                            </button>
                        </article>
                    ))}
                </div>
            )}

            {/* Create modal */}
            {creating && (
                <div className="modal-overlay animate-fade-in" onClick={() => setCreating(false)}>
                    <div className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <KeyRound size={16} />
                                New Solder Key
                            </h3>
                            <button onClick={() => setCreating(false)} className="text-(--base-06)"><X size={16} /></button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div>
                                <label className="input-label">Name</label>
                                <input
                                    type="text"
                                    value={name}
                                    onChange={e => setName(e.target.value)}
                                    onKeyDown={e => { if (e.key === 'Enter') handleCreate(); }}
                                    className="input-field w-full"
                                    placeholder="home-launcher"
                                    maxLength={128}
                                    autoFocus
                                />
                            </div>
                            <div>
                                <label className="input-label">Key from the Technic Platform</label>
                                <input
                                    type="text"
                                    value={platformKey}
                                    onChange={e => setPlatformKey(e.target.value)}
                                    onKeyDown={e => { if (e.key === 'Enter') handleCreate(); }}
                                    className="input-field input-mono w-full"
                                    placeholder="leave empty to generate one"
                                    maxLength={128}
                                    spellCheck={false}
                                    autoComplete="off"
                                />
                                <p className="text-xs text-(--base-06) mt-1.5">
                                    To link this Solder to technicpack.net, paste the key from your
                                    Technic profile under Edit Profile, Solder Configuration. Technic
                                    only ever verifies a key it issued itself, so a generated one
                                    cannot link it.
                                </p>
                                <p className="text-xs text-(--base-06) mt-1">
                                    Leave it empty for a launcher key instead: Core generates one and
                                    shows it once, and a launcher carries it to reach your private
                                    packs.
                                </p>
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setCreating(false)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runCreate(handleCreate)} disabled={creatingKey} className="btn btn-primary disabled:opacity-40">Create key</button>
                        </div>
                    </div>
                </div>
            )}

            {/* Plaintext reveal — shown once */}
            {revealedKey && (
                <div className="modal-overlay animate-fade-in" onClick={() => setRevealedKey(null)}>
                    <div className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--accent-light)">
                                <KeyRound size={16} />
                                {revealedKey.name} — copy now
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07) flex items-start gap-2">
                                <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                                This is the only time the full key will be shown. Copy it into your
                                launcher config now — it cannot be recovered afterwards.
                            </p>
                            <div className="flex items-center gap-2 p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-sm break-all">
                                <span className="flex-1">{revealedKey.plaintext}</span>
                                <button
                                    onClick={() => { navigator.clipboard.writeText(revealedKey.plaintext); showToast('Copied.', true); }}
                                    className="btn btn-secondary btn-sm shrink-0"
                                >
                                    <Copy size={12} />
                                    Copy
                                </button>
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setRevealedKey(null)} className="btn btn-primary">
                                <EyeOff size={12} />
                                Hide
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {/* Delete confirmation */}
            {deleting && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeleting(null)}>
                    <div className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                Delete {deleting.name}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Any launcher using this key will immediately lose access to your private packs.
                                This action cannot be undone.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeleting(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingKey} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </div>
                </div>
            )}

        </main>
    );
}
