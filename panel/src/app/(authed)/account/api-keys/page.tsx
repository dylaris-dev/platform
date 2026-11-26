"use client";

import Checkbox from '@/components/ui/Checkbox';
import Select from '@/components/ui/Select';
import React, { useCallback, useEffect, useState } from 'react';
import { Key, Plus, Copy, Trash2, AlertTriangle, Shield, X, EyeOff, RefreshCw, MoreVertical } from 'lucide-react';
import PageHeader from '@/components/ui/PageHeader';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import Menu, { type MenuEntry } from '@/components/ui/Menu';
import Callout from '@/components/server/Callout';
import { useAppData } from '@/lib/AppDataContext';
import { listAPIKeys, createAPIKey, revokeAPIKey, rollAPIKey, type APIKey, type APIKeyOptions } from '@/lib/api/apiKeys';
import { getCatalog, type CatalogScope } from '@/lib/api/authzCatalog';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDateTime } from '@/lib/time';

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
    const [loadError, setLoadError] = useState<string | null>(null);
    const [creating, setCreating] = useState(false);

    const [form, setForm] = useState<{
        name: string;
        servers: string[];
        permissions: string[];
        ratePerMin: number;
        expiresInDays: number;
    }>({ name: '', servers: [], permissions: [], ratePerMin: 60, expiresInDays: 0 });

    // Kept OUT of `form` so it is never carried across a second create: the
    // form is reset on success, and a password lingering in component state
    // after the dialog closes is a credential nobody asked us to hold.
    const [reauthPassword, setReauthPassword] = useState('');
    const [reauthCode, setReauthCode] = useState('');

    const [revealedKey, setRevealedKey] = useState<{ plaintext: string; name: string; rolled?: boolean } | null>(null);
    const [revoking, setRevoking] = useState<APIKey | null>(null);
    const [creatingKey, runCreate] = useBusy();
    const [revokingKey, runRevoke] = useBusy();
    const [rolling, setRolling] = useState<APIKey | null>(null);
    const [rollingKey, runRoll] = useBusy();

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const res = await listAPIKeys();
        if (res.success && res.keys) setKeys(res.keys);
        if (res.success && res.options) setOptions(res.options);
        setLoadError(res.success ? null : (res.message || 'Could not load API keys.'));
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
                        c => (!options?.allowedCaps || options.allowedCaps.includes(c.id))
                            && (!options?.keyCaps || options.keyCaps.includes(c.id)),
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
            expiresInDays: form.expiresInDays || undefined,
            password: reauthPassword,
            code: reauthCode.replace(/\s/g, '') || undefined,
        });
        if (res.success && res.plaintext) {
            setCreating(false);
            setForm({ name: '', servers: [], permissions: ['rcon.exec'], ratePerMin: 60, expiresInDays: 0 });
            setReauthPassword('');
            setReauthCode('');
            setRevealedKey({ plaintext: res.plaintext, name });
            refresh();
        } else {
            showToast(res.message || 'Create failed', false);
        }
    };

    const handleRoll = async () => {
        if (!rolling) return;
        const res = await rollAPIKey(rolling.id, reauthPassword, reauthCode.replace(/\s/g, '') || undefined);
        if (res.success && res.key) {
            setRevealedKey({ plaintext: res.key, name: rolling.name, rolled: true });
            setRolling(null);
            setReauthPassword('');
            setReauthCode('');
            refresh();
        } else {
            showToast(res.message || 'Roll failed', false);
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
        <main className="flex-1 overflow-y-auto app-page space-y-6">
            <PageHeader
                title="API Keys"
                description="Let scripts, bots and stream tools call Dylaris without your session."
                actions={
                    <button
                        onClick={() => setCreating(true)}
                        disabled={options?.enabled === false}
                        className="btn btn-primary btn-sm"
                    >
                        <Plus size={14} />
                        New API Key
                    </button>
                }
            />

            <Callout tone="info" icon={<Shield size={16} />} foldable title="How API keys work">
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
                <p className="mt-1 text-(--base-06)">
                    A key counts like an invited member: auditing is switched on for every server it
                    can reach, and its actions show in the server&apos;s audit log as
                    {' '}<code className="font-mono">key: &lt;name&gt; (owner &lt;you&gt;)</code>. Signing out
                    everywhere does not revoke keys; revoke them here.
                </p>
            </Callout>

            {/* The operator can turn user keys off entirely. Existing keys stop
                working at that moment (the switch is enforced at use, not only at
                mint), so the page has to say that rather than just hide the
                button and leave the listed keys looking live. */}
            {options?.enabled === false && (
                <Callout tone="warning" icon={<AlertTriangle size={16} />} title="API keys are turned off for users on this platform.">
                    New keys cannot be created, and any key listed below is already being
                    refused. An admin can re-enable them under Settings → Features.
                </Callout>
            )}

            <Section title="Your keys" description="Plaintext is shown once at creation; the list only carries metadata.">
                {loading ? (
                    <SkeletonList rows={3} />
                ) : loadError ? (
                    <EmptyState
                        tone="error"
                        icon={<AlertTriangle size={20} />}
                        title="Could not load API keys"
                        description={loadError}
                        action={<button onClick={() => { setLoading(true); refresh(); }} className="btn btn-secondary btn-sm">Retry</button>}
                    />
                ) : keys.length === 0 ? (
                    <EmptyState
                        icon={<Key size={20} />}
                        title="No API keys yet"
                        description="Create a key to let automation call Dylaris on your behalf."
                        action={options?.enabled === false ? undefined : (
                            <button onClick={() => setCreating(true)} className="btn btn-primary btn-sm"><Plus size={14} />New API Key</button>
                        )}
                    />
                ) : (
                    <div className="srv-surface">
                        <RowList label="API keys">
                            {keys.map(k => {
                                const expired = !!k.expiresAt && new Date(k.expiresAt).getTime() <= Date.now();
                                const rollable = !k.revokedAt && !expired;
                                const entries: MenuEntry[] = [
                                    ...(rollable ? [{ label: 'Roll', icon: <RefreshCw size={14} />, onSelect: () => setRolling(k) }] : []),
                                    ...(!k.revokedAt ? [{ label: 'Revoke', icon: <Trash2 size={14} />, danger: true, onSelect: () => setRevoking(k) }] : []),
                                ];
                                const serverNames = k.scope.servers.length === 0
                                    ? '(none)'
                                    : k.scope.servers.map(uuid => {
                                        const srv = servers.find(s => s.uuid === uuid);
                                        return srv ? srv.name : uuid.slice(0, 8) + '…';
                                    }).join(', ');
                                return (
                                    <Row
                                        key={k.id}
                                        leading={<Key size={16} className={k.revokedAt ? 'text-(--base-05)' : 'text-(--accent-light)'} />}
                                        title={
                                            <span className="flex flex-wrap items-center gap-2">
                                                <span>{k.name}</span>
                                                {k.revokedAt && <span className="srv-chip h-5 px-2 text-[11px]">revoked</span>}
                                                {!k.revokedAt && expired && <span className="srv-chip h-5 px-2 text-[11px]">expired</span>}
                                                <span className="srv-chip h-5 px-2 font-mono text-[11px]">{k.ratePerMin}/min</span>
                                            </span>
                                        }
                                        meta={
                                            <span className="block space-y-0.5 whitespace-normal">
                                                <span className="block"><span className="text-(--base-07)">Permissions:</span> {k.scope.permissions.join(', ') || '—'}</span>
                                                <span className="block"><span className="text-(--base-07)">Servers:</span> {serverNames}</span>
                                                <span className="block">
                                                    Expires: <span className="text-(--base-08)">{k.expiresAt ? fmtDateTime(k.expiresAt) : 'never'}</span>
                                                    {k.lastUsedAt && <> · Last used: <span className="text-(--base-08)">{fmtDateTime(k.lastUsedAt)}</span></>}
                                                </span>
                                            </span>
                                        }
                                        actions={entries.length > 0 ? (
                                            <Menu align="end" trigger={<button type="button" aria-label={`Actions for ${k.name}`} className="app-icon-btn"><MoreVertical size={16} /></button>} items={entries} />
                                        ) : undefined}
                                    />
                                );
                            })}
                        </RowList>
                    </div>
                )}
            </Section>

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
                                    maxLength={64}
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
                                                                        <Checkbox
                                                                            ariaLabel={c.label}
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
                                                    <Checkbox
                                                        ariaLabel={s.name}
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
                                    max={600}
                                    value={form.ratePerMin}
                                    onChange={e => setForm({ ...form, ratePerMin: parseInt(e.target.value || '60', 10) })}
                                    className="input-field input-mono w-32"
                                />
                            </div>

                            <div>
                                <label className="input-label" htmlFor="apikey-expiry">Expires</label>
                                <Select
                                    id="apikey-expiry"
                                    className="w-48"
                                    value={String(form.expiresInDays)}
                                    onChange={v => setForm({ ...form, expiresInDays: parseInt(v, 10) })}
                                    options={[
                                        { value: '30', label: 'In 30 days' },
                                        { value: '90', label: 'In 90 days' },
                                        { value: '365', label: 'In a year' },
                                        { value: '0', label: 'Never' },
                                    ]}
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
                            {revealedKey.rolled && (
                                <p className="text-sm text-(--base-07)">
                                    The previous key keeps working for 24 hours so you can switch over
                                    without downtime. Rolling again before then ends it immediately.
                                </p>
                            )}
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

            {/* Roll confirmation: re-authenticates like create */}
            {rolling && (
                <div className="modal-overlay animate-fade-in" onClick={() => { setRolling(null); setReauthPassword(''); setReauthCode(''); }}>
                    <ModalPanel onClose={() => { setRolling(null); setReauthPassword(''); setReauthCode(''); }} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <RefreshCw size={16} />
                                Roll {rolling.name}?
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07)">
                                A new key is created and shown once. The current key keeps working for
                                24 hours, then stops.
                            </p>
                            <p className="text-sm text-(--base-07)">
                                Suspect the key leaked? Revoke it instead: a rolled key keeps working for 24 hours.
                            </p>
                            <ReauthFields
                                idPrefix="apikey-roll"
                                twoFactorEnabled={!!user?.is2FAEnabled}
                                password={reauthPassword}
                                code={reauthCode}
                                onPassword={setReauthPassword}
                                onCode={setReauthCode}
                                disabled={rollingKey}
                            />
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => { setRolling(null); setReauthPassword(''); setReauthCode(''); }} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runRoll(handleRoll)} disabled={rollingKey || !reauthReady(!!user?.is2FAEnabled, reauthPassword, reauthCode)} className="btn btn-primary disabled:opacity-40">Roll key</button>
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
