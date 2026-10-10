"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Key, Plus, Copy, Trash2, AlertTriangle, Shield, X, EyeOff } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { listAPIKeys, createAPIKey, revokeAPIKey, type APIKey, type APIKeyOptions } from '@/lib/api/apiKeys';
import { getCatalog, type CatalogScope } from '@/lib/api/authzCatalog';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';

// per-user API key management. Lives under /account/ because
// keys are owned by users, not the admin platform. Plaintext is shown
// exactly once on creation; subsequent listings only carry metadata.
//
// The permission picker renders from the authz catalog, never from a list kept
// here. It used to hold a hardcoded one-entry array from back when rcon.exec
// was the only key-authed route, so every capability added to the external
// surface afterwards was unmintable from the panel while the backend accepted
// it perfectly well - the failure mode a frontend permission array always has.

export default function ApiKeysPage() {
    const { user, servers } = useAppData();
    const [keys, setKeys] = useState<APIKey[]>([]);
    const [options, setOptions] = useState<APIKeyOptions | null>(null);
    const [catalog, setCatalog] = useState<CatalogScope[]>([]);
    const [loading, setLoading] = useState(true);
    const [creating, setCreating] = useState(false);

    const [form, setForm] = useState<{
        name: string;
        servers: string[];
        permissions: string[];
        ratePerMin: number;
    }>({ name: '', servers: [], permissions: [], ratePerMin: 60 });

    // Kept OUT of `form` so it is never carried across a second create: the
    // form is reset on success, and a password lingering in component state
    // after the dialog closes is a credential nobody asked us to hold.
    const [reauthPassword, setReauthPassword] = useState('');
    const [reauthCode, setReauthCode] = useState('');

    const [revealedKey, setRevealedKey] = useState<{ plaintext: string; name: string } | null>(null);
    const [revoking, setRevoking] = useState<APIKey | null>(null);
    const [creatingKey, runCreate] = useBusy();
    const [revokingKey, runRevoke] = useBusy();

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const res = await listAPIKeys();
        if (res.success && res.keys) setKeys(res.keys);
        if (res.success && res.options) setOptions(res.options);
        setLoading(false);
    }, []);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        // PANEL capabilities are dropped: a key can never carry one (the backend
        // rejects them at mint), so offering them would only produce a 400.
        getCatalog().then(res => {
            if (res.success && res.catalog) setCatalog(res.catalog.filter(sc => sc.scope !== 'panel'));
        });
    }, []);

    // What this account may actually put on a key: the catalog, narrowed by the
    // operator whitelist when there is one. A NULL allowedCaps means the
    // operator set no whitelist, which is "no extra restriction" - treating it
    // like an empty list would show an empty picker on every default install.
    const mintable = catalog
        .map(sc => ({
            ...sc,
            categories: sc.categories
                .map(cat => ({
                    ...cat,
                    capabilities: cat.capabilities.filter(
                        c => !options?.allowedCaps || options.allowedCaps.includes(c.id),
                    ),
                }))
                .filter(cat => cat.capabilities.length > 0),
        }))
        .filter(sc => sc.categories.length > 0);

    const handleCreate = async () => {
        const name = form.name.trim();
        if (!name) { showToast('Name required', false); return; }
        if (form.permissions.length === 0) { showToast('Pick at least one permission', false); return; }
        const res = await createAPIKey({
            name,
            servers: form.servers,
            permissions: form.permissions,
            ratePerMin: form.ratePerMin,
            password: reauthPassword,
            code: reauthCode.replace(/\s/g, '') || undefined,
        });
        if (res.success && res.plaintext) {
            setCreating(false);
            setForm({ name: '', servers: [], permissions: ['rcon.exec'], ratePerMin: 60 });
            setReauthPassword('');
            setReauthCode('');
            setRevealedKey({ plaintext: res.plaintext, name });
            refresh();
        } else {
            showToast(res.message || 'Create failed', false);
        }
    };

    const handleRevoke = async () => {
        if (!revoking) return;
        const res = await revokeAPIKey(revoking.id);
        setRevoking(null);
        if (res.success) {
            showToast('Key revoked.', true);
            refresh();
        } else {
            showToast(res.message || 'Revoke failed', false);
        }
    };

    if (!user) return null;

    return (
        <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
            <header className="flex items-center gap-3 mb-4">
                <Key size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">API Keys</h1>
                <div className="ml-auto">
                    <button
                        onClick={() => setCreating(true)}
                        disabled={options?.enabled === false}
                        className="btn btn-primary btn-sm"
                    >
                        <Plus size={12} />
                        New API Key
                    </button>
                </div>
            </header>

            <div className="card p-4 mb-4 text-xs text-(--base-07) flex items-start gap-2">
                <Shield size={14} className="text-(--accent-light) shrink-0 mt-0.5" />
                <div>
                    <p>
                        Used by external automation (Discord bots, stream alerts, scripts) to call
                        Dylaris APIs without your session token. Authenticate with{' '}
                        <code className="font-mono">Authorization: Bearer dyl_…</code>. Each key is
                        scoped to a list of servers + capabilities.
                    </p>
                    <p className="mt-1 text-(--base-06)">
                        Base path: <code className="font-mono">/api/external</code>, servers addressed
                        by UUID — e.g. <code className="font-mono">POST /api/external/servers/&lt;uuid&gt;/power</code>
                        {' '}with <code className="font-mono">{'{ "action": "restart" }'}</code>.
                    </p>
                </div>
            </div>

            {/* The operator can turn user keys off entirely. Existing keys stop
                working at that moment (the switch is enforced at use, not only at
                mint), so the page has to say that rather than just hide the
                button and leave the listed keys looking live. */}
            {options?.enabled === false && (
                <div className="card p-4 mb-4 text-xs flex items-start gap-2">
                    <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <div>
                        <p className="text-(--base-08)">API keys are turned off for users on this platform.</p>
                        <p className="mt-1 text-(--base-06)">
                            New keys cannot be created, and any key listed below is already being
                            refused. An admin can re-enable them under Settings → Features.
                        </p>
                    </div>
                </div>
            )}

            {loading ? (
                <SkeletonList rows={3} />
            ) : keys.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <Key size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No API keys yet.</p>
                </div>
            ) : (
                <div className="space-y-2">
                    {keys.map(k => (
                        <article key={k.id} className="card p-3 flex items-start gap-3">
                            <Key size={16} className={k.revokedAt ? 'text-(--base-05)' : 'text-(--accent-light)'} />
                            <div className="min-w-0 flex-1">
                                <div className="flex items-center gap-2 flex-wrap">
                                    <span className="font-medium text-sm text-(--base-09)">{k.name}</span>
                                    {k.revokedAt && (
                                        <span className="mono-label bg-(--base-03) text-(--base-06) px-1.5 rounded-sm">revoked</span>
                                    )}
                                    <span className="mono-label bg-(--base-03) text-(--base-07) px-1.5 rounded-sm">
                                        {k.ratePerMin}/min
                                    </span>
                                </div>
                                <div className="mt-1 text-xs text-(--base-06)">
                                    <span className="text-(--base-07)">Permissions:</span> {k.scope.permissions.join(', ') || '—'}
                                </div>
                                <div className="text-xs text-(--base-06)">
                                    <span className="text-(--base-07)">Servers:</span>{' '}
                                    {k.scope.servers.length === 0
                                        ? <span className="text-(--base-05) italic">(none)</span>
                                        : k.scope.servers.map(uuid => {
                                            const srv = servers.find(s => s.uuid === uuid);
                                            return srv ? srv.name : uuid.slice(0, 8) + '…';
                                        }).join(', ')}
                                </div>
                                {k.lastUsedAt && (
                                    <div className="text-xs text-(--base-06) mt-0.5">
                                        Last used: <span className="text-(--base-08)">{new Date(k.lastUsedAt).toLocaleString()}</span>
                                    </div>
                                )}
                            </div>
                            {!k.revokedAt && (
                                <button onClick={() => setRevoking(k)} className="btn btn-secondary btn-sm shrink-0">
                                    <Trash2 size={12} className="text-(--error-light)" />
                                    Revoke
                                </button>
                            )}
                        </article>
                    ))}
                </div>
            )}

            {/* Create modal */}
            {creating && (
                <div className="modal-overlay animate-fade-in" onClick={() => setCreating(false)}>
                    <ModalPanel onClose={() => setCreating(false)} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        {/* .modal-header sets padding and a divider, no layout, so a
                            header with a title AND a close button stacks them and the X
                            lands under the heading. Every modal that carries one adds
                            the row itself; this matches AssignOrphanModal. */}
                        <div className="modal-header flex items-start justify-between gap-3">
                            <h3 className="modal-title flex items-center gap-2">
                                <Key size={16} />
                                New API Key
                            </h3>
                            <button
                                onClick={() => setCreating(false)}
                                aria-label="Close"
                                className="shrink-0 p-1 rounded text-(--base-06) hover:bg-(--base-03) hover:text-(--base-08) transition-colors"
                            >
                                <X size={16} />
                            </button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div>
                                <label className="input-label">Name</label>
                                <input
                                    type="text"
                                    value={form.name}
                                    onChange={e => setForm({ ...form, name: e.target.value })}
                                    className="input-field w-full"
                                    placeholder="discord-bot-rcon"
                                    maxLength={128}
                                />
                            </div>

                            <div>
                                <label className="input-label">Permissions</label>
                                {mintable.length === 0 ? (
                                    <p className="text-xs text-(--base-06) mt-1">
                                        No permissions are available to put on a key. An admin has narrowed the list to capabilities you do not hold.
                                    </p>
                                ) : (
                                    <div className="mt-1 max-h-64 overflow-y-auto border border-(--base-04) rounded-md p-2 space-y-3">
                                        {mintable.map(sc => (
                                            <div key={sc.scope} className="space-y-2">
                                                <div className="mono-label text-(--base-06)">
                                                    {sc.scope === 'server' ? 'Per server' : 'Account-wide'}
                                                </div>
                                                {sc.categories.map(cat => (
                                                    <div key={cat.category}>
                                                        <div className="text-xs text-(--base-07) mb-1">{cat.category}</div>
                                                        <div className="space-y-0.5">
                                                            {cat.capabilities.map(c => {
                                                                const checked = form.permissions.includes(c.id);
                                                                return (
                                                                    <label key={c.id} className="flex items-center gap-2 px-2 py-1 rounded-md cursor-pointer hover:bg-(--base-03)">
                                                                        <input
                                                                            type="checkbox"
                            className="checkbox"
                                                                            checked={checked}
                                                                            onChange={() => setForm({
                                                                                ...form,
                                                                                permissions: checked
                                                                                    ? form.permissions.filter(x => x !== c.id)
                                                                                    : [...form.permissions, c.id],
                                                                            })}
                                                                        />
                                                                        <span className="text-sm text-(--base-09)">{c.label}</span>
                                                                        <code className="text-xs text-(--base-06) font-mono ml-auto">{c.id}</code>
                                                                    </label>
                                                                );
                                                            })}
                                                        </div>
                                                    </div>
                                                ))}
                                            </div>
                                        ))}
                                    </div>
                                )}
                                {/* Per-server capabilities are inert without a server, and the
                                    backend refuses that combination at mint rather than issuing
                                    a key whose only safeguard is the use-time check. Say so here
                                    instead of letting the create fail. */}
                                {form.servers.length === 0 && form.permissions.some(p => mintable.find(sc => sc.scope === 'server')?.categories.some(cat => cat.capabilities.some(c => c.id === p))) && (
                                    <p className="flex items-start gap-1.5 text-xs text-(--warning-light) mt-2">
                                        <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                                        <span>Per-server permissions need at least one server selected below.</span>
                                    </p>
                                )}
                            </div>

                            <div>
                                <label className="input-label">Server scope</label>
                                {servers.length === 0 ? (
                                    <p className="text-xs text-(--base-06) mt-1">You don&apos;t own any servers — nothing to scope.</p>
                                ) : (
                                    <div className="space-y-1 mt-1 max-h-40 overflow-y-auto border border-(--base-04) rounded-md p-1">
                                        {servers.map(s => {
                                            const checked = form.servers.includes(s.uuid);
                                            return (
                                                <label key={s.uuid} className="flex items-center gap-2 px-2 py-1 rounded-md cursor-pointer hover:bg-(--base-03)">
                                                    <input
                                                        type="checkbox"
                            className="checkbox"
                                                        checked={checked}
                                                        onChange={() => setForm({
                                                            ...form,
                                                            servers: checked
                                                                ? form.servers.filter(x => x !== s.uuid)
                                                                : [...form.servers, s.uuid],
                                                        })}
                                                    />
                                                    <span className="text-sm text-(--base-09)">{s.name}</span>
                                                    <span className="text-xs text-(--base-06) font-mono ml-auto">{s.uuid.slice(0, 8)}…</span>
                                                </label>
                                            );
                                        })}
                                    </div>
                                )}
                            </div>

                            <div>
                                <label className="input-label">Rate limit (req/min)</label>
                                <input
                                    type="number"
                                    min={1}
                                    max={1000}
                                    value={form.ratePerMin}
                                    onChange={e => setForm({ ...form, ratePerMin: parseInt(e.target.value || '60', 10) })}
                                    className="input-field input-mono w-32"
                                />
                            </div>
                        </div>
                        <ReauthFields
                            idPrefix="apikey"
                            twoFactorEnabled={!!user?.is2FAEnabled}
                            password={reauthPassword}
                            code={reauthCode}
                            onPassword={setReauthPassword}
                            onCode={setReauthCode}
                            disabled={creatingKey}
                        />
                        <div className="modal-footer">
                            <button onClick={() => { setCreating(false); setReauthPassword(''); setReauthCode(''); }} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runCreate(handleCreate)} disabled={creatingKey || !reauthReady(!!user?.is2FAEnabled, reauthPassword, reauthCode)} className="btn btn-primary disabled:opacity-40">Create key</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Plaintext reveal — shown once */}
            {revealedKey && (
                <div className="modal-overlay animate-fade-in" onClick={() => setRevealedKey(null)}>
                    <ModalPanel onClose={() => setRevealedKey(null)} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--accent-light)">
                                <Key size={16} />
                                {revealedKey.name} — copy now
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07) flex items-start gap-2">
                                <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                                This is the only time the full key will be shown. Copy it into your
                                secret store now.
                            </p>
                            <div className="flex items-center gap-2 p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-sm break-all">
                                <span className="flex-1">{revealedKey.plaintext}</span>
                                <button
                                    onClick={() => { navigator.clipboard.writeText(revealedKey.plaintext); showToast('Copied.', true); }}
                                    className="btn btn-secondary btn-sm"
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
                    </ModalPanel>
                </div>
            )}

            {/* Revoke confirmation */}
            {revoking && (
                <div className="modal-overlay animate-fade-in" onClick={() => setRevoking(null)}>
                    <ModalPanel onClose={() => setRevoking(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={18} />
                                Revoke {revoking.name}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Existing calls using this key will fail with 401 immediately.
                                Revoked keys stay listed for audit but can&apos;t be reactivated.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setRevoking(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runRevoke(handleRevoke)} disabled={revokingKey} className="btn btn-danger disabled:opacity-40">Revoke</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

        </main>
    );
}
