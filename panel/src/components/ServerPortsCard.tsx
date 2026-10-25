"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Check, Loader2, Pencil, Plug, Plus, Trash2, X } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import CopyText from '@/components/ui/CopyText';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import {
    getServerPorts, createServerPort, updateServerPort, deleteServerPort, serverPower,
    type ServerPort, type ServerPortsView, type CreateServerPortRequest,
} from '@/lib/api';
import { parseHostPort, portAddress, portUsage, portsError } from '@/lib/serverPorts';

const NAME_MAX = 40;
const NOTE_MAX = 200;

/**
 * The extra ports of one server, next to its Direct address in the header.
 *
 * Renders nothing in gateway routing, when the read fails (no network.read),
 * and when the feature is closed to this user and the server holds no port:
 * a button that only opens "not enabled" on every server would be noise on a
 * platform that never turned the feature on.
 */
export default function ServerPortsCard({ serverId, canWrite }: { serverId: number; canWrite: boolean }) {
    const [view, setView] = useState<ServerPortsView | null>(null);
    const [open, setOpen] = useState(false);

    const load = useCallback(async () => {
        const res = await getServerPorts(serverId).catch(() => null);
        setView(res?.success && res.ports ? res.ports : null);
    }, [serverId]);

    useEffect(() => { void load(); }, [load]);

    if (!view || view.routingMode === 'gateway') return null;
    if (!view.allowed && !view.isAdmin && view.ports.length === 0) return null;

    return (
        <>
            <button
                type="button"
                onClick={() => setOpen(true)}
                title="Extra ports"
                className="focus-ring inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-mono bg-(--base-03)/50 border border-(--base-04) text-(--base-07) hover:border-(--base-05) hover:text-(--base-09) active:bg-(--base-03) transition-colors"
            >
                <Plug size={11} className="text-(--base-06) shrink-0" aria-hidden="true" />
                Ports {view.ports.length}
            </button>
            {open && (
                <ServerPortsModal
                    serverId={serverId}
                    view={view}
                    canWrite={canWrite}
                    reload={load}
                    onClose={() => setOpen(false)}
                />
            )}
        </>
    );
}

function ServerPortsModal({ serverId, view, canWrite, reload, onClose }: {
    serverId: number;
    view: ServerPortsView;
    canWrite: boolean;
    reload: () => Promise<void>;
    onClose: () => void;
}) {
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);
    const [pendingRestart, setPendingRestart] = useState(false);
    const [editing, setEditing] = useState<number | null>(null);
    const [editName, setEditName] = useState('');
    const [editNote, setEditNote] = useState('');

    const canAdd = canWrite && (view.allowed || view.isAdmin);

    const offerRestart = async () => {
        const ok = await confirmDialog({
            title: 'Restart now?',
            message: 'Players are disconnected.',
            confirmLabel: 'Restart',
            destructive: true,
        });
        if (!ok) return;
        setBusy(true);
        try {
            const res = await serverPower(serverId, 'restart');
            if (res?.success === false) {
                setError(res.message || res.error || 'The server could not be restarted.');
                return;
            }
            setPendingRestart(false);
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    const changed = async () => {
        setPendingRestart(true);
        await reload();
        await offerRestart();
    };

    const remove = async (p: ServerPort) => {
        const ok = await confirmDialog({
            title: `Remove ${p.name}?`,
            message: `Port ${p.hostPort} stops reaching this server on its next restart.`,
            confirmLabel: 'Remove',
        });
        if (!ok) return;
        setError('');
        setBusy(true);
        const res = await deleteServerPort(serverId, p.id).catch(() => null);
        setBusy(false);
        if (!res?.success) {
            setError(portsError(res, 'The port could not be removed.'));
            return;
        }
        await changed();
    };

    const startEdit = (p: ServerPort) => {
        setEditing(p.id);
        setEditName(p.name);
        setEditNote(p.note);
        setError('');
    };

    const saveEdit = async (p: ServerPort) => {
        setBusy(true);
        const res = await updateServerPort(serverId, p.id, editName.trim(), editNote.trim()).catch(() => null);
        setBusy(false);
        if (!res?.success) {
            setError(portsError(res, 'The port could not be saved.'));
            return;
        }
        setEditing(null);
        toast('Port saved');
        await reload();
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-2xl flex flex-col max-h-[85vh]" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <div>
                        <h3 className="modal-title flex items-center gap-2">
                            <Plug size={16} className="text-(--accent-light)" />
                            Extra ports
                        </h3>
                        <p className="text-xs text-(--base-07) mt-1">
                            {portUsage(view.used, view.limit)} used by the owner
                            {view.range && <> &middot; range {view.range}</>}
                        </p>
                    </div>
                    <button type="button" onClick={onClose} aria-label="Close" className="p-1 text-(--base-06) hover:text-(--base-09) focus-visible:text-(--base-09) transition-colors">
                        <X size={18} />
                    </button>
                </div>

                <div className="modal-body flex-1 overflow-y-auto space-y-4">
                    <p className="text-sm text-(--base-07)">
                        Ports beyond the game port, for plugins such as voice chat or a map&apos;s web view. Changes apply on the next restart.
                    </p>

                    {!view.allowed && !view.isAdmin && (
                        <p className="alert alert-info text-xs">Extra ports are not enabled for this server. Ask an administrator.</p>
                    )}

                    {view.ports.length === 0 ? (
                        <p className="text-xs text-(--base-06)">No extra ports yet.</p>
                    ) : (
                        <div className="space-y-2">
                            {view.ports.map(p => (
                                <div key={p.id} className="bg-(--base-02) rounded-md px-3 py-2 border border-(--base-03)">
                                    {editing === p.id ? (
                                        <form
                                            className="flex flex-wrap items-end gap-2"
                                            onSubmit={e => { e.preventDefault(); void saveEdit(p); }}
                                        >
                                            <label className="flex flex-col gap-1 flex-1 min-w-32">
                                                <span className="mono-label">Name</span>
                                                <input value={editName} onChange={e => setEditName(e.target.value)} maxLength={NAME_MAX} className="input-field text-sm" autoFocus />
                                            </label>
                                            <label className="flex flex-col gap-1 flex-[2] min-w-40">
                                                <span className="mono-label">Note</span>
                                                <input value={editNote} onChange={e => setEditNote(e.target.value)} maxLength={NOTE_MAX} className="input-field text-sm" />
                                            </label>
                                            <button type="submit" className="btn btn-primary btn-sm" disabled={busy || editName.trim() === ''}>
                                                <Check size={13} aria-hidden="true" /> Save
                                            </button>
                                            <button type="button" className="btn btn-secondary btn-sm" onClick={() => setEditing(null)} disabled={busy}>
                                                Cancel
                                            </button>
                                        </form>
                                    ) : (
                                        <div className="flex items-center gap-3">
                                            <div className="min-w-0 flex-1">
                                                <div className="flex flex-wrap items-center gap-2">
                                                    <span className="text-sm font-medium text-(--base-09)">{p.name}</span>
                                                    <span className="badge badge-neutral">{p.protocol.toUpperCase()}</span>
                                                    {p.preset && <span className="badge badge-accent">Voice chat</span>}
                                                </div>
                                                <div className="flex flex-wrap items-center gap-2 mt-1">
                                                    <CopyText value={portAddress(view.address, p.hostPort)} className="text-xs font-mono text-(--base-08)" />
                                                    <span className="mono-label">-&gt; container port {p.containerPort}</span>
                                                </div>
                                                {p.note && <p className="text-xs text-(--base-06) mt-1 break-words">{p.note}</p>}
                                            </div>
                                            {canWrite && (
                                                <div className="flex items-center gap-1 shrink-0">
                                                    <button
                                                        type="button"
                                                        onClick={() => startEdit(p)}
                                                        disabled={busy}
                                                        className="p-1 rounded text-(--base-06) hover:text-(--base-09) hover:bg-(--base-03) focus-visible:text-(--base-09) disabled:opacity-40 transition-colors"
                                                        aria-label={`Edit ${p.name}`}
                                                    >
                                                        <Pencil size={14} />
                                                    </button>
                                                    <button
                                                        type="button"
                                                        onClick={() => remove(p)}
                                                        disabled={busy}
                                                        className="p-1 rounded text-(--error-light) hover:bg-(--error-ghost) focus-visible:bg-(--error-ghost) disabled:opacity-40 transition-colors"
                                                        aria-label={`Remove ${p.name}`}
                                                    >
                                                        <Trash2 size={14} />
                                                    </button>
                                                </div>
                                            )}
                                        </div>
                                    )}
                                </div>
                            ))}
                        </div>
                    )}

                    {pendingRestart && (
                        <p className="text-xs text-(--warning-light)">
                            Restart the server to apply.{' '}
                            <button
                                type="button"
                                onClick={offerRestart}
                                disabled={busy}
                                className="underline underline-offset-2 hover:text-(--base-09) focus-visible:text-(--base-09) disabled:opacity-40"
                            >
                                Restart now
                            </button>
                        </p>
                    )}
                    {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}

                    {canAdd && (
                        <AddPortForm
                            serverId={serverId}
                            voiceAvailable={view.routingMode === 'ip_port' && !view.ports.some(p => p.preset)}
                            isAdmin={view.isAdmin}
                            range={view.range}
                            onAdded={changed}
                        />
                    )}
                </div>
            </ModalPanel>
        </div>
    );
}

function AddPortForm({ serverId, voiceAvailable, isAdmin, range, onAdded }: {
    serverId: number;
    voiceAvailable: boolean;
    isAdmin: boolean;
    range: string;
    onAdded: () => Promise<void>;
}) {
    const [kind, setKind] = useState<'custom' | 'voice'>('custom');
    const [protocol, setProtocol] = useState<'tcp' | 'udp'>('tcp');
    const [containerPort, setContainerPort] = useState('');
    const [name, setName] = useState('');
    const [note, setNote] = useState('');
    const [hostPort, setHostPort] = useState('');
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');

    const voice = voiceAvailable && kind === 'voice';
    const cp = Number(containerPort);
    const cpOk = Number.isInteger(cp) && cp >= 1 && cp <= 65535;
    const hp = parseHostPort(hostPort);
    const valid = hp !== 'invalid' && (voice || (cpOk && name.trim() !== ''));

    const submit = async () => {
        if (!valid) return;
        setError('');
        setSaving(true);
        const req: CreateServerPortRequest = voice
            ? { preset: 'svc', name: name.trim(), note: note.trim() }
            : { protocol, containerPort: cp, name: name.trim(), note: note.trim() };
        if (isAdmin && typeof hp === 'number') req.hostPort = hp;
        const res = await createServerPort(serverId, req).catch(() => null);
        setSaving(false);
        if (!res?.success) {
            setError(portsError(res, 'The port could not be added.'));
            return;
        }
        toast(`Port ${res.port?.hostPort ?? ''} added`.trim());
        setContainerPort('');
        setName('');
        setNote('');
        setHostPort('');
        setKind('custom');
        await onAdded();
    };

    return (
        <form
            className="bg-(--base-03) rounded-md border border-(--base-04) p-3 space-y-3"
            onSubmit={e => { e.preventDefault(); void submit(); }}
        >
            <div className="mono-label">Add port</div>
            <div className="flex flex-wrap gap-2">
                {voiceAvailable && (
                    <label className="flex flex-col gap-1">
                        <span className="mono-label">Type</span>
                        <select value={kind} onChange={e => setKind(e.target.value as 'custom' | 'voice')} className="input-field text-sm w-64">
                            <option value="custom">Custom</option>
                            <option value="voice">Voice chat (Simple Voice Chat or Plasmo Voice)</option>
                        </select>
                    </label>
                )}
                {!voice && (
                    <>
                        <label className="flex flex-col gap-1">
                            <span className="mono-label">Protocol</span>
                            <select value={protocol} onChange={e => setProtocol(e.target.value as 'tcp' | 'udp')} className="input-field text-sm w-24">
                                <option value="tcp">TCP</option>
                                <option value="udp">UDP</option>
                            </select>
                        </label>
                        <label className="flex flex-col gap-1">
                            <span className="mono-label">Port inside the server</span>
                            <input
                                value={containerPort}
                                onChange={e => setContainerPort(e.target.value)}
                                inputMode="numeric"
                                className="input-field input-mono text-sm w-40"
                                placeholder="e.g. 8100"
                                aria-invalid={containerPort !== '' && !cpOk}
                            />
                        </label>
                    </>
                )}
                {isAdmin && (
                    <label className="flex flex-col gap-1">
                        <span className="mono-label">Host port (admin)</span>
                        <input
                            value={hostPort}
                            onChange={e => setHostPort(e.target.value)}
                            inputMode="numeric"
                            className="input-field input-mono text-sm w-40"
                            placeholder={range ? `from ${range}` : 'from the range'}
                            aria-invalid={hp === 'invalid'}
                        />
                    </label>
                )}
            </div>
            {voice && (
                <p className="text-xs text-(--base-06)">
                    UDP to container port 24454. The mod&apos;s config is set to the public address. The mod must be installed and the server started once.
                </p>
            )}
            {isAdmin && <p className="text-xs text-(--base-06)">Leave the host port empty to assign one from the range. A named one may be any port from 1024 to 65535.</p>}
            <div className="flex flex-wrap gap-2">
                <label className="flex flex-col gap-1 flex-1 min-w-32">
                    <span className="mono-label">Name</span>
                    <input value={name} onChange={e => setName(e.target.value)} maxLength={NAME_MAX} className="input-field text-sm" placeholder={voice ? 'Voice chat' : 'e.g. Dynmap'} />
                </label>
                <label className="flex flex-col gap-1 flex-[2] min-w-40">
                    <span className="mono-label">Note</span>
                    <input value={note} onChange={e => setNote(e.target.value)} maxLength={NOTE_MAX} className="input-field text-sm" placeholder="Optional" />
                </label>
            </div>
            {containerPort !== '' && !cpOk && !voice && <p className="text-xs text-(--error-light)">The port inside the server is a number from 1 to 65535.</p>}
            {hp === 'invalid' && <p className="text-xs text-(--error-light)">A host port is a number from 1024 to 65535.</p>}
            {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}
            <button type="submit" className="btn btn-primary btn-sm" disabled={saving || !valid}>
                {saving ? <Loader2 size={13} className="animate-spin" aria-hidden="true" /> : <Plus size={13} aria-hidden="true" />}
                Add port
            </button>
        </form>
    );
}
