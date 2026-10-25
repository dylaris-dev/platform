"use client";

import { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle, Loader2, Pencil, Plug, Plus, Search, Trash2 } from 'lucide-react';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import { LimitField, LimitHelp, limitLabel } from '@/components/settings/LimitField';
import { SwitchRow } from '@/components/ui/Switch';
import { toast } from '@/components/ui/Toast';
import {
    getPortsOverview, setPortsSettings, setNodePortRange,
    getPortPermissions, setPortPermission, deletePortPermission,
    getPortLimits, setPortLimit, deletePortLimit,
    type PortsSettings, type PortsNodeUsage, type ServerPortRow, type PortPermission, type PortLimit,
} from '@/lib/api';
import { formatMaxPerUser, parseMaxPerUser, portsError } from '@/lib/serverPorts';

type ScopeKind = 'server' | 'user' | 'role';
const KIND_LABEL: Record<ScopeKind, string> = { server: 'Server', user: 'User', role: 'Role' };

/** Core's own shape check (same as the voice overrides): server and role ids
 *  are positive integers, user ids free text without spaces or colons. */
function validId(kind: ScopeKind, id: string): boolean {
    if (kind === 'user') return id.length > 0 && id.length <= 64 && !/[: ]/.test(id);
    return /^[1-9][0-9]*$/.test(id);
}

/**
 * Extra ports per server, direct routing only: the global switch, range and
 * per-user default, who may (overrides), how many (limits), and what is taken.
 */
export default function ExtraPortsCard() {
    const [rows, setRows] = useState<ServerPortRow[] | null>(null);
    const [nodes, setNodes] = useState<PortsNodeUsage[]>([]);
    const [stored, setStored] = useState<PortsSettings | null>(null);
    const [loadError, setLoadError] = useState('');
    const [q, setQ] = useState('');
    const firstLoad = useRef(true);

    const load = useCallback(async (query: string) => {
        const res = await getPortsOverview(query).catch(() => null);
        if (!res?.success || !res.settings) {
            setLoadError(portsError(res, 'The extra ports could not be loaded.'));
            return;
        }
        setLoadError('');
        setRows(res.ports ?? []);
        setNodes(res.nodes ?? []);
        setStored(res.settings);
    }, []);

    // Debounced search; the first load runs at once.
    useEffect(() => {
        const t = setTimeout(() => { void load(q.trim()); }, firstLoad.current ? 0 : 300);
        firstLoad.current = false;
        return () => clearTimeout(t);
    }, [q, load]);

    const reload = () => load(q.trim());

    return (
        <>
            <SettingsSection stored={stored} loadError={loadError} onSaved={s => setStored(s)} />
            <AccessSection />
            <LimitsSection />
            <SettingsCard
                title="Extra ports in use"
                description="Usage of each node's range, and every extra port. TCP and UDP each have the whole range."
                icon={Plug}
            >
                {loadError ? (
                    <div className="alert alert-error text-xs" role="alert">{loadError}</div>
                ) : rows === null ? (
                    <p className="flex items-center gap-2 text-xs text-(--base-06)">
                        <Loader2 size={13} className="animate-spin" aria-hidden="true" /> Loading...
                    </p>
                ) : (
                    <>
                        <SettingsGroup title="Nodes" first>
                            {nodes.length === 0 ? (
                                <p className="text-xs text-(--base-06)">No nodes yet.</p>
                            ) : (
                                <div className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                                    {nodes.map(n => <NodeRangeRow key={n.nodeId} node={n} onSaved={reload} />)}
                                </div>
                            )}
                        </SettingsGroup>
                        <SettingsGroup title="Ports">
                            <label className="relative block max-w-sm">
                                <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-06)" aria-hidden="true" />
                                <input
                                    value={q}
                                    onChange={e => setQ(e.target.value)}
                                    maxLength={100}
                                    className="input-field input-icon-left text-sm w-full"
                                    placeholder="Search port, server, owner or name"
                                    aria-label="Search extra ports"
                                />
                            </label>
                            {rows.length === 0 ? (
                                <p className="text-xs text-(--base-06)">{q.trim() ? 'No port matches the search.' : 'No extra ports yet.'}</p>
                            ) : (
                                <div className="overflow-x-auto rounded-md border border-(--base-03)">
                                    <table className="w-full text-xs">
                                        <thead>
                                            <tr className="text-left text-(--base-06) border-b border-(--base-03)">
                                                {['Port', 'Protocol', 'Server', 'Owner', 'Node', 'Name', 'Note'].map(h => (
                                                    <th key={h} className="mono-label px-3 py-2 font-normal">{h}</th>
                                                ))}
                                            </tr>
                                        </thead>
                                        <tbody className="divide-y divide-(--base-03)">
                                            {rows.map(r => (
                                                <tr key={r.id} className="text-(--base-08)">
                                                    <td className="px-3 py-2 font-mono">{r.hostPort} <span className="text-(--base-06)">-&gt; {r.containerPort}</span></td>
                                                    <td className="px-3 py-2 font-mono">{r.protocol.toUpperCase()}</td>
                                                    <td className="px-3 py-2">{r.serverName} <span className="text-(--base-06)">#{r.serverId}</span></td>
                                                    <td className="px-3 py-2">{r.ownerName || r.ownerId}</td>
                                                    <td className="px-3 py-2">{r.nodeName}</td>
                                                    <td className="px-3 py-2">
                                                        {r.name}
                                                        {r.preset && <span className="badge badge-accent ml-1.5">Voice chat</span>}
                                                    </td>
                                                    <td className="px-3 py-2 text-(--base-06) max-w-56 truncate" title={r.note}>{r.note}</td>
                                                </tr>
                                            ))}
                                        </tbody>
                                    </table>
                                </div>
                            )}
                        </SettingsGroup>
                    </>
                )}
            </SettingsCard>
        </>
    );
}

function SettingsSection({ stored, loadError, onSaved }: {
    stored: PortsSettings | null;
    loadError: string;
    onSaved: (s: PortsSettings) => void;
}) {
    const [enabled, setEnabled] = useState(false);
    const [range, setRange] = useState('');
    const [max, setMax] = useState<number | null>(2);
    const [saving, setSaving] = useState(false);

    const reset = useCallback(() => {
        if (!stored) return;
        setEnabled(stored.enabled);
        setRange(stored.range);
        setMax(parseMaxPerUser(stored.maxPerUser));
    }, [stored]);
    useEffect(reset, [reset]);

    const patch: Partial<PortsSettings> = {};
    if (stored) {
        if (enabled !== stored.enabled) patch.enabled = enabled;
        if (range.trim() !== stored.range) patch.range = range.trim();
        if (formatMaxPerUser(max) !== formatMaxPerUser(parseMaxPerUser(stored.maxPerUser))) patch.maxPerUser = formatMaxPerUser(max);
    }
    const dirty = Object.keys(patch).length > 0;

    const save = async () => {
        setSaving(true);
        const res = await setPortsSettings(patch).catch(() => null);
        setSaving(false);
        if (!res?.success || !res.settings) {
            toast(portsError(res, 'The settings could not be saved.'), false);
            return false;
        }
        onSaved(res.settings);
        toast('Extra ports saved');
        return true;
    };

    return (
        <SettingsCard
            title="Extra ports"
            description="Lets server owners publish ports beyond the game port in direct routing, for voice chat, a map's web view or a query port. Changes reach a server on its next restart."
            icon={Plug}
            form={{ dirty, saving, loadFailed: !!loadError && !stored, save, discard: reset }}
        >
            <SwitchRow
                label="Extra ports for everyone"
                description="The default for users without an override below. Admins can always add ports."
                checked={enabled}
                disabled={!stored}
                onChange={setEnabled}
            />
            <SettingsGroup title="Range">
                <label className="flex flex-col gap-1 max-w-xs">
                    <span className="mono-label">Global range</span>
                    <input
                        value={range}
                        onChange={e => setRange(e.target.value)}
                        className="input-field input-mono text-sm"
                        placeholder="e.g. 26000-26099"
                        disabled={!stored}
                    />
                </label>
                <p className="text-xs text-(--base-06)">
                    Host ports users get, lowest free first. Empty means users cannot get one. Must lie in 1024-65535, outside the game port range, and be open in the node&apos;s firewall for TCP and UDP.
                </p>
            </SettingsGroup>
            <SettingsGroup title="Default per user">
                <div className="flex flex-wrap items-center gap-3">
                    <LimitField value={max} onChange={setMax} />
                    <span className="text-xs text-(--base-06)">Extra ports one server owner may hold across all servers. Product default is 2.</span>
                </div>
            </SettingsGroup>
        </SettingsCard>
    );
}

function NodeRangeRow({ node, onSaved }: { node: PortsNodeUsage; onSaved: () => Promise<void> }) {
    const [editing, setEditing] = useState(false);
    const [value, setValue] = useState('');
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    const save = async (range: string) => {
        setBusy(true);
        setError('');
        const res = await setNodePortRange(node.nodeId, range).catch(() => null);
        setBusy(false);
        if (!res?.success) {
            setError(portsError(res, 'The range could not be saved.'));
            return;
        }
        setEditing(false);
        toast(range ? 'Node range saved' : 'Node uses the global range');
        await onSaved();
    };

    return (
        <div className="p-3 space-y-2 text-sm">
            <div className="flex flex-wrap items-center gap-3">
                <span className="font-medium text-(--base-09) min-w-32">{node.nodeName}</span>
                <code className="input-mono text-xs text-(--base-08)">{node.range || 'no range'}</code>
                {node.range && <span className={`badge ${node.ownRange ? 'badge-accent' : 'badge-neutral'}`}>{node.ownRange ? 'Own' : 'Global'}</span>}
                {node.gameRange && <span className="mono-label">Game {node.gameRange}</span>}
                <span className="ml-auto flex items-center gap-2">
                    {node.size > 0 && (
                        <>
                            <span className="text-xs font-mono text-(--base-07)">{node.used} / {node.size}</span>
                            <span className="w-24 h-1.5 rounded-full bg-(--base-03) overflow-hidden" aria-hidden="true">
                                <span
                                    className={`block h-full ${node.percent >= 80 ? 'bg-(--warning)' : 'bg-(--accent)'}`}
                                    style={{ width: `${Math.min(100, node.percent)}%` }}
                                />
                            </span>
                            <span className="text-xs font-mono text-(--base-06) w-12 text-right">{node.percent}%</span>
                        </>
                    )}
                    {!editing && (
                        <button
                            type="button"
                            className="btn btn-secondary btn-sm"
                            onClick={() => { setValue(node.ownRange ? node.range : ''); setEditing(true); }}
                            aria-label={`Set the range of ${node.nodeName}`}
                        >
                            <Pencil size={13} aria-hidden="true" /> Range
                        </button>
                    )}
                </span>
            </div>
            {node.conflict && (
                <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                    <AlertTriangle size={12} className="mt-0.5 shrink-0" aria-hidden="true" />
                    <span>The range overlaps this node&apos;s game ports {node.gameRange}. Give the node its own range.</span>
                </p>
            )}
            {editing && (
                <form className="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void save(value.trim()); }}>
                    <label className="flex flex-col gap-1">
                        <span className="mono-label">Own range</span>
                        <input value={value} onChange={e => setValue(e.target.value)} className="input-field input-mono text-sm w-44" placeholder="e.g. 27000-27099" autoFocus />
                    </label>
                    <button type="submit" className="btn btn-primary btn-sm" disabled={busy || value.trim() === ''}>
                        {busy && <Loader2 size={13} className="animate-spin" aria-hidden="true" />} Save
                    </button>
                    {node.ownRange && (
                        <button type="button" className="btn btn-secondary btn-sm" disabled={busy} onClick={() => save('')}>
                            Use global
                        </button>
                    )}
                    <button type="button" className="btn btn-secondary btn-sm" disabled={busy} onClick={() => { setEditing(false); setError(''); }}>
                        Cancel
                    </button>
                </form>
            )}
            {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}
        </div>
    );
}

/** The scope picker both override lists share. */
function ScopeInputs({ kinds, kind, setKind, id, setId }: {
    kinds: ScopeKind[];
    kind: ScopeKind;
    setKind: (k: ScopeKind) => void;
    id: string;
    setId: (v: string) => void;
}) {
    const trimmed = id.trim();
    return (
        <>
            <label className="flex flex-col gap-1">
                <span className="mono-label">Scope</span>
                <select value={kind} onChange={e => setKind(e.target.value as ScopeKind)} className="input-field text-sm w-28">
                    {kinds.map(k => <option key={k} value={k}>{KIND_LABEL[k]}</option>)}
                </select>
            </label>
            <label className="flex flex-col gap-1 flex-1 min-w-36">
                <span className="mono-label">{KIND_LABEL[kind]} ID</span>
                <input
                    value={id}
                    onChange={e => setId(e.target.value)}
                    className="input-field input-mono text-sm"
                    placeholder={kind === 'user' ? 'user id' : 'e.g. 42'}
                    aria-invalid={trimmed !== '' && !validId(kind, trimmed)}
                />
            </label>
        </>
    );
}

function ScopeIdHint({ kind, id }: { kind: ScopeKind; id: string }) {
    const t = id.trim();
    if (t === '' || validId(kind, t)) return null;
    return (
        <p className="text-xs text-(--error-light)">
            {kind === 'user' ? 'A user ID has no spaces or colons.' : `A ${kind} ID is a whole number above 0.`}
        </p>
    );
}

function ScopeLabel({ scope }: { scope: string }) {
    const [k, ...rest] = scope.split(':');
    return (
        <>
            <span className="mono-label text-(--base-06) w-14">{KIND_LABEL[k as ScopeKind] ?? k}</span>
            <code className="input-mono text-xs text-(--base-08)">{rest.join(':')}</code>
        </>
    );
}

function RemoveButton({ scope, busy, onClick }: { scope: string; busy: boolean; onClick: () => void }) {
    return (
        <button
            type="button"
            className="btn btn-secondary btn-sm ml-auto"
            disabled={busy}
            onClick={onClick}
            aria-label={`Remove the override for ${scope}`}
        >
            {busy ? <Loader2 size={13} className="animate-spin" aria-hidden="true" /> : <Trash2 size={13} aria-hidden="true" />}
            Remove
        </button>
    );
}

function AccessSection() {
    const [rows, setRows] = useState<PortPermission[] | null>(null);
    const [loadError, setLoadError] = useState('');
    const [kind, setKind] = useState<ScopeKind>('server');
    const [id, setId] = useState('');
    const [allowed, setAllowed] = useState(true);
    const [adding, setAdding] = useState(false);
    const [removing, setRemoving] = useState<string | null>(null);

    const load = useCallback(async () => {
        const res = await getPortPermissions().catch(() => null);
        if (!res?.success || !Array.isArray(res.permissions)) {
            setLoadError(portsError(res, 'The extra port overrides could not be loaded.'));
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
        const res = await setPortPermission(`${kind}:${trimmed}`, allowed).catch(() => null);
        setAdding(false);
        if (!res?.success) {
            toast(portsError(res, 'The override could not be saved.'), false);
            return;
        }
        toast('Override saved');
        setId('');
        await load();
    };

    const remove = async (scope: string) => {
        setRemoving(scope);
        const res = await deletePortPermission(scope).catch(() => null);
        setRemoving(null);
        if (!res?.success) {
            toast(portsError(res, 'The override could not be removed.'), false);
            return;
        }
        toast('Override removed');
        await load();
    };

    return (
        <SettingsCard
            title="Extra ports access"
            description="Overrides the switch above for one server, user or role. Server beats user beats role. A deny stops new ports; existing ones stay until removed."
            icon={Plug}
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
                    {rows.map(r => (
                        <div key={r.scope} className="flex flex-wrap items-center gap-3 p-3 text-sm">
                            <ScopeLabel scope={r.scope} />
                            <span className={`badge ${r.allowed ? 'badge-success' : 'badge-error'}`}>{r.allowed ? 'Allowed' : 'Denied'}</span>
                            <RemoveButton scope={r.scope} busy={removing === r.scope} onClick={() => remove(r.scope)} />
                        </div>
                    ))}
                </div>
            )}
            <form className="flex flex-wrap items-end gap-2 pt-3 border-t border-(--base-03)" onSubmit={e => { e.preventDefault(); void add(); }}>
                <ScopeInputs kinds={['server', 'user', 'role']} kind={kind} setKind={setKind} id={id} setId={setId} />
                <label className="flex flex-col gap-1">
                    <span className="mono-label">Extra ports</span>
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
            <ScopeIdHint kind={kind} id={id} />
        </SettingsCard>
    );
}

function LimitsSection() {
    const [rows, setRows] = useState<PortLimit[] | null>(null);
    const [global, setGlobal] = useState('');
    const [loadError, setLoadError] = useState('');
    const [kind, setKind] = useState<ScopeKind>('user');
    const [id, setId] = useState('');
    const [max, setMax] = useState<number | null>(2);
    const [adding, setAdding] = useState(false);
    const [removing, setRemoving] = useState<string | null>(null);

    const load = useCallback(async () => {
        const res = await getPortLimits().catch(() => null);
        if (!res?.success || !Array.isArray(res.limits)) {
            setLoadError(portsError(res, 'The extra port limits could not be loaded.'));
            return;
        }
        setLoadError('');
        setRows(res.limits);
        setGlobal(res.global ?? '');
    }, []);
    useEffect(() => { void load(); }, [load]);

    const trimmed = id.trim();
    const idOk = validId(kind, trimmed);

    const add = async () => {
        if (!idOk) return;
        setAdding(true);
        const res = await setPortLimit(`${kind}:${trimmed}`, max).catch(() => null);
        setAdding(false);
        if (!res?.success) {
            toast(portsError(res, 'The limit could not be saved.'), false);
            return;
        }
        toast('Limit saved');
        setId('');
        await load();
    };

    const remove = async (scope: string) => {
        setRemoving(scope);
        const res = await deletePortLimit(scope).catch(() => null);
        setRemoving(null);
        if (!res?.success) {
            toast(portsError(res, 'The limit could not be removed.'), false);
            return;
        }
        toast('Limit removed');
        await load();
    };

    return (
        <SettingsCard
            title="Extra port limits"
            description={`How many extra ports one server owner may hold, per user or role. User beats role beats the default${global ? ` (${limitLabel(parseMaxPerUser(global))})` : ''}. Removing a row hands the question to the next scope.`}
            help={LimitHelp}
            icon={Plug}
        >
            {loadError ? (
                <div className="alert alert-error text-xs" role="alert">{loadError}</div>
            ) : rows === null ? (
                <p className="flex items-center gap-2 text-xs text-(--base-06)">
                    <Loader2 size={13} className="animate-spin" aria-hidden="true" /> Loading limits...
                </p>
            ) : rows.length === 0 ? (
                <p className="text-xs text-(--base-06)">No limits. Every owner follows the default.</p>
            ) : (
                <div className="rounded-md border border-(--base-03) divide-y divide-(--base-03)">
                    {rows.map(r => (
                        <div key={r.scope} className="flex flex-wrap items-center gap-3 p-3 text-sm">
                            <ScopeLabel scope={r.scope} />
                            <span className="badge badge-neutral">{limitLabel(r.maxPorts)}</span>
                            <RemoveButton scope={r.scope} busy={removing === r.scope} onClick={() => remove(r.scope)} />
                        </div>
                    ))}
                </div>
            )}
            <form className="flex flex-wrap items-end gap-2 pt-3 border-t border-(--base-03)" onSubmit={e => { e.preventDefault(); void add(); }}>
                <ScopeInputs kinds={['user', 'role']} kind={kind} setKind={setKind} id={id} setId={setId} />
                <div className="flex flex-col gap-1">
                    <span className="mono-label">Max ports</span>
                    <LimitField value={max} onChange={setMax} />
                </div>
                <button type="submit" className="btn btn-primary btn-sm" disabled={adding || !idOk}>
                    {adding ? <Loader2 size={13} className="animate-spin" aria-hidden="true" /> : <Plus size={13} aria-hidden="true" />}
                    Add
                </button>
            </form>
            <ScopeIdHint kind={kind} id={id} />
        </SettingsCard>
    );
}
