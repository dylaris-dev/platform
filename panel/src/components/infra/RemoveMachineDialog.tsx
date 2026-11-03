"use client";

import { useEffect, useState } from 'react';
import { AlertTriangle, Loader2, Trash2 } from 'lucide-react';
import { getMyNodeContents, deleteMyNode, type MyNodeServer } from '@/lib/api/myNodes';

/**
 * Removing your own machine, in two steps.
 *
 * Not confirmDialog: that one takes a single sentence on purpose, and this
 * needs a choice inside it - whether the servers go too. That choice is the
 * whole decision, so it cannot be a second button people read as a repeat of
 * the first.
 *
 * Step two exists only when something irreversible is about to happen, and it
 * NAMES what: every server and every sub-server, one per line. A count is a
 * number people click past; the world somebody forgot they had is the one they
 * would have wanted back.
 */
export default function RemoveMachineDialog({
    nodeId, nodeLabel, onClose, onRemoved,
}: {
    nodeId: number;
    nodeLabel: string;
    onClose: () => void;
    onRemoved: () => void;
}) {
    const [servers, setServers] = useState<MyNodeServer[] | null>(null);
    const [withServers, setWithServers] = useState(false);
    const [step, setStep] = useState<1 | 2>(1);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    useEffect(() => {
        let cancelled = false;
        getMyNodeContents(nodeId).then(res => {
            if (cancelled) return;
            if (!res.success) { setError(res.message || 'Could not read what is on this machine.'); return; }
            setServers(res.servers ?? []);
        });
        return () => { cancelled = true; };
    }, [nodeId]);

    // Escape closes, like every other dismissible surface in the panel.
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !busy) onClose(); };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [onClose, busy]);

    const loading = servers === null && !error;
    const count = servers?.length ?? 0;
    // With servers on it the machine cannot be removed on its own - Core
    // refuses. Saying so here beats a round trip that comes back as a refusal.
    const blocked = count > 0 && !withServers;

    const remove = async () => {
        setBusy(true);
        setError('');
        const res = await deleteMyNode(nodeId, withServers);
        setBusy(false);
        if (!res.success) { setError(res.message || 'Could not remove this machine.'); return; }
        onRemoved();
    };

    return (
        <div
            className="fixed inset-0 z-[70] flex items-center-safe justify-center-safe overflow-y-auto bg-black/60 p-4"
            role="dialog"
            aria-modal="true"
            aria-labelledby="remove-machine-title"
            onClick={e => { if (e.target === e.currentTarget && !busy) onClose(); }}
        >
            <div className="w-full max-w-lg rounded-lg border border-(--base-03) bg-(--base-01) shadow-xl">
                <div className="flex items-start gap-3 p-5 pb-3">
                    <AlertTriangle size={18} className="mt-0.5 shrink-0 text-(--error-light)" aria-hidden="true" />
                    <div className="min-w-0">
                        <h2 id="remove-machine-title" className="text-sm font-medium text-(--base-09)">
                            {step === 1 ? `Remove ${nodeLabel}?` : 'This cannot be undone'}
                        </h2>
                        <p className="mt-1 text-xs text-(--base-07)">
                            {step === 1
                                ? 'It stops being part of your account and frees the slot, so you can set a machine up again here or somewhere else.'
                                : 'Everything listed below is deleted permanently. There is no backup taken as part of this.'}
                        </p>
                    </div>
                </div>

                <div className="max-h-[45vh] overflow-y-auto px-5 pb-1">
                    {loading && (
                        <div className="flex items-center gap-2 py-3 text-xs text-(--base-06)">
                            <Loader2 size={13} className="animate-spin" /> Reading what is on it...
                        </div>
                    )}

                    {step === 1 && !loading && (
                        <>
                            {count === 0 ? (
                                <p className="py-1 text-xs text-(--base-06)">Nothing is running on it.</p>
                            ) : (
                                <label className="flex cursor-pointer items-start gap-2.5 rounded-md border border-(--base-03) bg-(--base-02) p-3">
                                    <input
                                        type="checkbox"
                                        checked={withServers}
                                        onChange={e => setWithServers(e.target.checked)}
                                        className="mt-0.5 accent-(--error)"
                                    />
                                    <span className="min-w-0 text-xs">
                                        <span className="text-(--base-09)">
                                            Delete the {count} server{count === 1 ? '' : 's'} on it as well
                                        </span>
                                        <span className="mt-1 block text-(--base-06)">
                                            Worlds, configuration and uploads included. Leave this unticked
                                            and move them to another machine first if you want to keep them.
                                        </span>
                                    </span>
                                </label>
                            )}
                            {blocked && (
                                <p className="mt-2 text-xs text-(--warning-light)">
                                    A machine cannot be removed while it still holds servers. Tick the box,
                                    or move them elsewhere first.
                                </p>
                            )}
                            {/* The containers on the machine are NOT stopped by this. Saying so
                                here is the difference between a clean decommission and someone
                                wondering why their server is still using the CPU. */}
                            <p className="mt-2 text-xs text-(--base-06)">
                                Anything already running on the machine keeps running until you stop it
                                there. A machine you set up here later will not pick it back up.
                            </p>
                        </>
                    )}

                    {step === 2 && (
                        <ul className="space-y-2 py-1">
                            {(servers ?? []).map(s => (
                                <li key={s.id} className="rounded-md border border-(--error)/30 bg-(--error-ghost) p-3">
                                    <div className="text-xs font-medium text-(--base-09)">{s.name}</div>
                                    <div className="mono-label mt-0.5 text-(--base-06)">{s.uuid}</div>
                                    {s.subServers.length > 0 && (
                                        <ul className="mt-1.5 space-y-0.5">
                                            {s.subServers.map(sub => (
                                                <li key={sub} className="font-mono text-[11px] text-(--base-07)">
                                                    {sub}
                                                    {sub === s.activeSubServer && (
                                                        <span className="ml-1.5 text-(--base-06)">· running</span>
                                                    )}
                                                </li>
                                            ))}
                                        </ul>
                                    )}
                                </li>
                            ))}
                        </ul>
                    )}

                    {error && <p className="py-2 text-xs text-(--error-light)">{error}</p>}
                </div>

                <div className="flex justify-end gap-2 border-t border-(--base-03) p-4">
                    <button type="button" onClick={onClose} disabled={busy} className="btn btn-secondary btn-sm disabled:opacity-40">
                        Cancel
                    </button>
                    {step === 1 ? (
                        <button
                            type="button"
                            disabled={loading || blocked || busy}
                            onClick={() => (withServers ? setStep(2) : remove())}
                            className="btn btn-danger btn-sm disabled:opacity-40"
                        >
                            {busy ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                            {withServers ? 'Continue' : 'Remove machine'}
                        </button>
                    ) : (
                        <button type="button" disabled={busy} onClick={remove} className="btn btn-danger btn-sm disabled:opacity-40">
                            {busy ? <Loader2 size={13} className="animate-spin" /> : <Trash2 size={13} />}
                            Delete permanently
                        </button>
                    )}
                </div>
            </div>
        </div>
    );
}
