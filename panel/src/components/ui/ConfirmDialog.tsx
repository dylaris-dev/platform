'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle } from 'lucide-react';

/**
 * Replacement for window.confirm on destructive actions.
 *
 * The native dialog is OS chrome on a dark panel and blocks the JS thread, but
 * the reason this is a correctness fix and not a cosmetic one: a browser that has
 * suppressed further dialogs for the page - Chrome offers exactly that after a
 * few - returns false without asking. The action then silently does not happen
 * and the user is told nothing.
 *
 * Deliberately a module-level singleton driven by one <ConfirmDialogRoot/> in the
 * authed layout, rather than a per-component hook: there are ~19 call sites spread
 * over 14 files, and this way each changes by an import and an `await` instead of
 * also having to thread a node into its JSX. The markup is the hand-rolled confirm
 * that was already inside RoutesModal, so nothing about the look changes.
 */
export interface ConfirmOptions {
    title: string;
    /** Body text. A plain string: every call site is one sentence. */
    message: string;
    /** Defaults to "Delete", which most callers are. Set it when they are not. */
    confirmLabel?: string;
    cancelLabel?: string;
    /** false renders the primary button instead of the danger one. */
    destructive?: boolean;
}

interface PendingConfirm extends ConfirmOptions {
    resolve: (ok: boolean) => void;
}

let publish: ((p: PendingConfirm | null) => void) | null = null;

/**
 * confirmDialog is the drop-in for `if (!confirm(...)) return;` - the call site
 * keeps its control flow and gains an await.
 *
 * Fails CLOSED: with no <ConfirmDialogRoot/> mounted there is nothing to ask, so
 * it resolves false and the destructive action does not run. Same direction the
 * native confirm fails, which is the one that cannot destroy anything.
 */
export function confirmDialog(o: ConfirmOptions): Promise<boolean> {
    if (!publish) {
        console.error('confirmDialog called with no <ConfirmDialogRoot/> mounted; refusing the action');
        return Promise.resolve(false);
    }
    return new Promise<boolean>((resolve) => publish!({ ...o, resolve }));
}

/** Mount once, above everything that can ask for a confirmation. */
export function ConfirmDialogRoot() {
    const [pending, setPending] = useState<PendingConfirm | null>(null);
    const confirmRef = useRef<HTMLButtonElement>(null);

    useEffect(() => {
        publish = setPending;
        return () => {
            publish = null;
        };
    }, []);

    const close = useCallback((ok: boolean) => {
        setPending((p) => {
            p?.resolve(ok);
            return null;
        });
    }, []);

    // Escape cancels and focus starts on the confirm button, so the dialog stays
    // operable from the keyboard the way the native one was.
    useEffect(() => {
        if (!pending) return;
        // Back to the button that asked, so a confirm opened over a modal
        // leaves the keyboard in that modal rather than on the page body.
        const opener = document.activeElement as HTMLElement | null;
        confirmRef.current?.focus();
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape') close(false);
        };
        window.addEventListener('keydown', onKey);
        return () => {
            window.removeEventListener('keydown', onKey);
            if (opener && document.contains(opener)) opener.focus();
        };
    }, [pending, close]);

    if (!pending) return null;

    const destructive = pending.destructive !== false;

    return (
        <div className="modal-overlay animate-fade-in" onClick={() => close(false)}>
            <div
                className="modal-panel max-w-sm"
                role="alertdialog"
                aria-modal="true"
                aria-labelledby="confirm-dialog-title"
                onClick={(e) => e.stopPropagation()}
            >
                <div className="modal-header">
                    <h3
                        id="confirm-dialog-title"
                        className={`modal-title flex items-center gap-2 ${destructive ? 'text-(--error-light)' : ''}`}
                    >
                        {destructive && <AlertTriangle size={16} />} {pending.title}
                    </h3>
                </div>
                <div className="modal-body">
                    <p className="text-sm text-(--base-07)">{pending.message}</p>
                </div>
                <div className="modal-footer">
                    <button onClick={() => close(false)} className="btn btn-secondary">
                        {pending.cancelLabel ?? 'Cancel'}
                    </button>
                    <button
                        ref={confirmRef}
                        onClick={() => close(true)}
                        className={`btn ${destructive ? 'btn-danger' : 'btn-primary'}`}
                    >
                        {pending.confirmLabel ?? 'Delete'}
                    </button>
                </div>
            </div>
        </div>
    );
}
