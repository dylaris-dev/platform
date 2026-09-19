"use client";

import { useCallback, useEffect, useState } from 'react';
import { KeyRound, Loader2 } from 'lucide-react';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import {
    MyNodeJoinAttempt, admitMyNode, getMyNodeJoinAttempt, resetMyNodePairing, shortFingerprint,
} from '@/lib/api/myNodes';

// The owner's own re-pairing of a machine. Before this, a machine whose key was
// wedged or leaked could only be removed and added again: a new identity, a
// node slot, and its servers orphaned. The operator's Reset pairing needs a
// panel capability no customer holds.

/** The row action: take the machine's login away. */
export function ResetPairingButton({ nodeId, label, onDone }: { nodeId: number; label: string; onDone: (msg: string, ok: boolean) => void }) {
    const [busy, setBusy] = useState(false);
    const reset = async () => {
        if (!(await confirmDialog({
            title: `Reset pairing of ${label}?`,
            message: 'Its key is refused and its secret cleared at once. The machine keeps retrying with a new key, '
                + 'which you then admit here after comparing it with the machine\'s log. Once back it restarts its '
                + 'servers, which disconnects their players. Use this when its key is stuck or may have leaked.',
            confirmLabel: 'Reset pairing',
        }))) return;
        setBusy(true);
        const res = await resetMyNodePairing(nodeId);
        setBusy(false);
        onDone(res.success ? (res.note || 'Pairing reset.') : (res.message || 'Could not reset pairing'), !!res.success);
    };
    return (
        <button
            type="button"
            onClick={reset}
            disabled={busy}
            title={`Reset pairing of ${label}`}
            aria-label={`Reset pairing of ${label}`}
            className="shrink-0 text-(--base-06) hover:text-(--base-09) p-1.5 rounded-md transition-colors disabled:opacity-40
                       focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
        >
            {busy ? <Loader2 size={14} className="animate-spin" /> : <KeyRound size={14} />}
        </button>
    );
}

/** The fingerprint as the owner copied it, reduced to lowercase hex; "" if it is not one. */
export function normalizeFingerprint(s: string): string {
    const v = s.trim().toLowerCase().replace(/-/g, '');
    return /^[0-9a-f]+$/.test(v) ? v : '';
}

/**
 * Shown under a machine that is not connected: what is knocking as it, and the
 * way to let the owner's own key in.
 *
 * The owner pastes the fingerprint from the machine itself. What is knocking is
 * shown only as a hint: anyone who learns the machine's id can knock, and could
 * keep that line showing their own key.
 */
export function PendingAdmission({ nodeId, onDone }: { nodeId: number; onDone: (msg: string, ok: boolean) => void }) {
    const [attempt, setAttempt] = useState<MyNodeJoinAttempt | null>(null);
    const [typed, setTyped] = useState('');
    const [busy, setBusy] = useState(false);

    const load = useCallback(async () => {
        const res = await getMyNodeJoinAttempt(nodeId);
        setAttempt(res.success ? (res.attempt ?? null) : null);
    }, [nodeId]);
    useEffect(() => {
        load();
        const t = setInterval(load, 20000);
        return () => clearInterval(t);
    }, [load]);

    if (!attempt) return null;
    const knocking = attempt.presentedKey ?? '';
    const fp = normalizeFingerprint(typed);
    const valid = fp.length === 16 || fp.length === 64;
    const matches = valid && knocking.startsWith(fp);
    const waiting = !!attempt.approvedUntil && new Date(attempt.approvedUntil).getTime() > Date.now();

    const admit = async () => {
        setBusy(true);
        const res = await admitMyNode(nodeId, fp);
        setBusy(false);
        onDone(res.success ? (res.note || 'Admitted.') : (res.message || 'Could not admit the machine'), !!res.success);
        if (res.success) setTyped('');
        await load();
    };

    return (
        <div className="mt-2 rounded-md border border-(--warning)/30 bg-(--warning)/5 px-3 py-2.5 text-xs space-y-2">
            <p className="text-(--warning-light)">
                {waiting
                    ? 'Admitted. Waiting for the machine to connect with that key.'
                    : 'This machine is trying to connect and is not admitted.'}
            </p>
            <p className="text-(--base-07)">
                On the machine, find the line <code className="font-mono">nodekey: this node&apos;s key fingerprint is</code> in
                the node&apos;s log and paste the value. Only that key is let in.
                {knocking && <> Knocking right now: <code className="font-mono text-(--base-09)">{shortFingerprint(knocking)}</code>.</>}
            </p>
            <div className="flex flex-col sm:flex-row gap-2 sm:items-center">
                <label htmlFor={`fp-${nodeId}`} className="sr-only">Key fingerprint from the machine&apos;s log</label>
                <input
                    id={`fp-${nodeId}`}
                    className="input-field text-xs font-mono w-full sm:max-w-xs"
                    placeholder="abcd-ef01-2345-6789"
                    value={typed}
                    onChange={e => setTyped(e.target.value)}
                    aria-invalid={typed !== '' && !valid ? true : undefined}
                />
                <button type="button" onClick={admit} disabled={busy || !valid} className="btn btn-secondary btn-sm disabled:opacity-60">
                    {busy ? 'Admitting…' : 'Admit this key'}
                </button>
            </div>
            {valid && knocking && !matches && (
                <p className="text-(--warning-light)">
                    That is not the key knocking right now. If the machine is yours and running, check you copied the
                    line from the right machine.
                </p>
            )}
        </div>
    );
}
