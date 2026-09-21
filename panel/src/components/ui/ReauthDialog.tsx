'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { ShieldAlert } from 'lucide-react';
import { ReauthFields, reauthReady } from '@/components/ReauthFields';

/**
 * Asks the acting administrator for their own credential before an action that
 * hands somebody durable access.
 *
 * Core requires this on seven admin routes - setting an account's password,
 * removing its second factor, changing its address, creating a privileged
 * account, and the role, permission-flag and panel-role grants. Measured on
 * production before it did: an admin session alone, with no password, could
 * take over any account on the platform. The session-kill that covers a
 * password change does not reach it, because it ends the VICTIM's sessions
 * while the borrowed admin session is the thing asking.
 *
 * A singleton driven by one <ReauthDialogRoot/> in the authed layout, exactly
 * like ConfirmDialog next door and for the same reason: the call sites keep
 * their control flow and gain an await instead of each threading a node into
 * its JSX.
 */
export interface ReauthResult {
    password: string;
    code: string;
}

export interface ReauthOptions {
    /** What the administrator is about to do, e.g. "Reset this password". */
    title: string;
    /** One sentence naming the account and the consequence. */
    message: string;
    /** Whether the ACTING account has 2FA, which decides if a code is asked for. */
    twoFactorEnabled: boolean;
    confirmLabel?: string;
}

interface PendingReauth extends ReauthOptions {
    resolve: (r: ReauthResult | null) => void;
}

let publish: ((p: PendingReauth | null) => void) | null = null;

/**
 * reauthDialog resolves with the credential, or null when the administrator
 * cancels - so a call site reads `const r = await reauthDialog(...); if (!r) return;`.
 *
 * Fails CLOSED: with no root mounted there is nobody to ask, so it resolves
 * null and the action does not run. Sending nothing would only earn a 401 from
 * Core anyway; refusing here says why.
 */
export function reauthDialog(o: ReauthOptions): Promise<ReauthResult | null> {
    if (!publish) {
        console.error('reauthDialog called with no <ReauthDialogRoot/> mounted; refusing the action');
        return Promise.resolve(null);
    }
    return new Promise<ReauthResult | null>((resolve) => publish!({ ...o, resolve }));
}

/** Mount once, above everything that can ask for a re-authentication. */
export function ReauthDialogRoot() {
    const [pending, setPending] = useState<PendingReauth | null>(null);
    const [password, setPassword] = useState('');
    const [code, setCode] = useState('');
    const panelRef = useRef<HTMLDivElement>(null);

    useEffect(() => {
        publish = setPending;
        return () => {
            publish = null;
        };
    }, []);

    const close = useCallback((r: ReauthResult | null) => {
        setPending((p) => {
            p?.resolve(r);
            return null;
        });
        // Never leave a typed password sitting in state behind a closed dialog.
        setPassword('');
        setCode('');
    }, []);

    useEffect(() => {
        if (!pending) return;
        panelRef.current?.querySelector<HTMLInputElement>('input[type="password"]')?.focus();
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape') close(null);
        };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [pending, close]);

    if (!pending) return null;

    const ready = reauthReady(pending.twoFactorEnabled, password, code);
    const submit = () => {
        if (ready) close({ password, code });
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={() => close(null)}>
            <div
                ref={panelRef}
                className="modal-panel max-w-sm"
                role="alertdialog"
                aria-modal="true"
                aria-labelledby="reauth-dialog-title"
                onClick={(e) => e.stopPropagation()}
                onKeyDown={(e) => {
                    if (e.key === 'Enter') submit();
                }}
            >
                <div className="modal-header">
                    <h3 id="reauth-dialog-title" className="modal-title flex items-center gap-2">
                        <ShieldAlert size={16} /> {pending.title}
                    </h3>
                </div>
                <div className="modal-body space-y-3">
                    <p className="text-sm text-(--base-07)">{pending.message}</p>
                    <ReauthFields
                        twoFactorEnabled={pending.twoFactorEnabled}
                        password={password}
                        code={code}
                        onPassword={setPassword}
                        onCode={setCode}
                        idPrefix="reauth-dialog"
                    />
                </div>
                <div className="modal-footer">
                    <button onClick={() => close(null)} className="btn btn-secondary">Cancel</button>
                    <button
                        onClick={submit}
                        disabled={!ready}
                        className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed"
                    >
                        {pending.confirmLabel ?? 'Confirm'}
                    </button>
                </div>
            </div>
        </div>
    );
}
