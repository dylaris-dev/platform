"use client";

import { useCallback, useEffect, useState } from 'react';
import { Loader2, Mic, Plus, Trash2 } from 'lucide-react';
import SettingsCard from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';
import { getVoicePermissions, setVoicePermission, deleteVoicePermission, type VoicePermission } from '@/lib/api';

type ScopeKind = 'server' | 'user' | 'role';

const KIND_LABEL: Record<ScopeKind, string> = { server: 'Server', user: 'User', role: 'Role' };

/** Core's own shape check, so a typo is refused here with a sentence instead of
 *  a bare 400. Server and role ids are positive integers, user ids free text. */
function validId(kind: ScopeKind, id: string): boolean {
    if (kind === 'user') return id.length > 0 && id.length <= 64 && !/[: ]/.test(id);
    return /^[1-9][0-9]*$/.test(id);
}

/**
 * Who may turn voice chat on, beyond the global switch above. A server row
 * beats a user row beats a role row beats the global switch. Saved per row,
 * like the per-tenant traffic overrides, so the card has no Save button.
 */
export default function VoiceAccessCard() {
    const [rows, setRows] = useState<VoicePermission[] | null>(null);
    const [loadError, setLoadError] = useState('');
    const [kind, setKind] = useState<ScopeKind>('server');
    const [id, setId] = useState('');
    const [allowed, setAllowed] = useState(true);
    const [adding, setAdding] = useState(false);
    const [removing, setRemoving] = useState<string | null>(null);

    const load = useCallback(async () => {
        const res = await getVoicePermissions().catch(() => null);
        if (!res || res.success === false || !Array.isArray(res.permissions)) {
            setLoadError(res?.message || res?.error || 'The voice chat overrides could not be loaded.');
            return;
        }
        setLoadError('');
        setRows(res.permissions);
    }, []);

    useEffect(() => { void load(); }, [load]);

    const trimmed = id.trim();
    const idOk = validId(kind, trimmed);

    const add = async () => {
        if (!idOk) return;
        setAdding(true);
        const res = await setVoicePermission(`${kind}:${trimmed}`, allowed).catch(() => null);
        setAdding(false);
        if (!res || res.success === false) {
            toast(res?.message || res?.error || 'The override could not be saved.', false);
            return;
        }
        toast('Override saved');
        setId('');
        await load();
    };

    const remove = async (scope: string) => {
        setRemoving(scope);
        const res = await deleteVoicePermission(scope).catch(() => null);
        setRemoving(null);
        if (!res || res.success === false) {
            toast(res?.message || res?.error || 'The override could not be removed.', false);
            return;
        }
        toast('Override removed');
        await load();
    };

    return (
        <SettingsCard
            title="Voice chat access"
            description="Overrides the voice chat switch above for one server, user or role. Server beats user beats role. A deny turns voice off on servers that have it within a minute."
            icon={Mic}
        >
            {loadError ? (
                <div className="alert alert-error text-xs" role="alert">{loadError}</div>
            ) : rows === null ? (
                <p className="flex items-center gap-2 text-xs text-(--base-06)">
                    <Loader2 size={13} className="animate-spin" aria-hidden="true" /> Loading overrides...
                </p>
            ) : rows.length === 0 ? (
                <p className="text-xs text-(--base-06)">No overrides. Every server follows the switch above.</p>
            ) : (
                <div className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                    {rows.map(r => {
                        const [k, ...rest] = r.scope.split(':');
                        return (
                            <div key={r.scope} className="flex flex-wrap items-center gap-3 p-3 text-sm">
                                <span className="mono-label text-(--base-06) w-14">{KIND_LABEL[k as ScopeKind] ?? k}</span>
                                <code className="input-mono text-xs text-(--base-08)">{rest.join(':')}</code>
                                <span className={`badge ${r.allowed ? 'badge-success' : 'badge-error'}`}>
                                    {r.allowed ? 'Allowed' : 'Denied'}
                                </span>
                                <button
                                    type="button"
                                    className="btn btn-secondary btn-sm ml-auto"
                                    disabled={removing === r.scope}
                                    onClick={() => remove(r.scope)}
                                    aria-label={`Remove the override for ${r.scope}`}
                                >
                                    {removing === r.scope
                                        ? <Loader2 size={13} className="animate-spin" aria-hidden="true" />
                                        : <Trash2 size={13} aria-hidden="true" />}
                                    Remove
                                </button>
                            </div>
                        );
                    })}
                </div>
            )}

            <form
                className="flex flex-wrap items-end gap-2 pt-3 border-t border-(--base-03)"
                onSubmit={e => { e.preventDefault(); void add(); }}
            >
                <label className="flex flex-col gap-1">
                    <span className="mono-label">Scope</span>
                    <select value={kind} onChange={e => setKind(e.target.value as ScopeKind)} className="input-field text-sm w-28">
                        <option value="server">Server</option>
                        <option value="user">User</option>
                        <option value="role">Role</option>
                    </select>
                </label>
                <label className="flex flex-col gap-1 flex-1 min-w-36">
                    <span className="mono-label">{kind === 'user' ? 'User ID' : kind === 'role' ? 'Role ID' : 'Server ID'}</span>
                    <input
                        value={id}
                        onChange={e => setId(e.target.value)}
                        className="input-field input-mono text-sm"
                        placeholder={kind === 'user' ? 'user id' : 'e.g. 42'}
                        aria-invalid={trimmed !== '' && !idOk}
                    />
                </label>
                <label className="flex flex-col gap-1">
                    <span className="mono-label">Voice chat</span>
                    <select value={allowed ? 'allow' : 'deny'} onChange={e => setAllowed(e.target.value === 'allow')} className="input-field text-sm w-28">
                        <option value="allow">Allow</option>
                        <option value="deny">Deny</option>
                    </select>
                </label>
                <button type="submit" className="btn btn-primary btn-sm" disabled={adding || !idOk}>
                    {adding ? <Loader2 size={13} className="animate-spin" aria-hidden="true" /> : <Plus size={13} aria-hidden="true" />}
                    Add
                </button>
            </form>
            {trimmed !== '' && !idOk && (
                <p className="text-xs text-(--error-light)">
                    {kind === 'user' ? 'A user ID has no spaces or colons.' : `A ${kind} ID is a whole number above 0.`}
                </p>
            )}
        </SettingsCard>
    );
}
