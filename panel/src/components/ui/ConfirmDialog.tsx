'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle } from 'lucide-react';
import {
    AlertDialog,
    AlertDialogBackdrop,
    AlertDialogClose,
    AlertDialogContent,
    AlertDialogDescription,
    AlertDialogTitle,
} from '@ui-sdk/react/alert-dialog';

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
    /** Body text. A plain string; a blank line in it starts a new paragraph. */
    message: string;
    /** Defaults to "Delete", which most callers are. Set it when they are not. */
    confirmLabel?: string;
    cancelLabel?: string;
    /** false renders the primary button instead of the danger one. */
    destructive?: boolean;
    /** Start keyboard focus on Cancel, for a confirm that is the riskier way on. */
    focusCancel?: boolean;
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
    const cancelRef = useRef<HTMLButtonElement>(null);

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

    // Back to the button that asked, so a confirm opened over a modal leaves
    // the keyboard in that modal rather than on the page body: the SDK only
    // restores focus to a trigger, and this dialog has none.
    useEffect(() => {
        if (!pending) return;
        const opener = document.activeElement as HTMLElement | null;
        return () => {
            if (opener && document.contains(opener)) opener.focus();
        };
    }, [pending]);

    if (!pending) return null;

    const destructive = pending.destructive !== false;

    return (
        <AlertDialog open onOpenChange={(open, details) => { if (!open) close(details.returnValue === 'confirm'); }}>
            <AlertDialogBackdrop className="modal-overlay animate-fade-in" onClick={() => close(false)} />
            <AlertDialogContent
                className="modal-panel fixed inset-0 m-auto h-fit w-[calc(100%-2rem)] max-w-sm"
                initialFocus={() => (pending.focusCancel ? cancelRef : confirmRef).current}
            >
                <div className="modal-header">
                    <AlertDialogTitle
                        className={`modal-title flex items-center gap-2 ${destructive ? 'text-(--error-light)' : ''}`}
                    >
                        {destructive && <AlertTriangle size={16} />} {pending.title}
                    </AlertDialogTitle>
                </div>
                <div className="modal-body">
                    {/* pre-line: a message may carry its steps as separate paragraphs. */}
                    <AlertDialogDescription className="text-sm text-(--base-07) whitespace-pre-line">
                        {pending.message}
                    </AlertDialogDescription>
                </div>
                <div className="modal-footer">
                    <AlertDialogClose ref={cancelRef} className="btn btn-secondary">
                        {pending.cancelLabel ?? 'Cancel'}
                    </AlertDialogClose>
                    <AlertDialogClose
                        ref={confirmRef}
                        value="confirm"
                        className={`btn ${destructive ? 'btn-danger' : 'btn-primary'}`}
                    >
                        {pending.confirmLabel ?? 'Delete'}
                    </AlertDialogClose>
                </div>
            </AlertDialogContent>
        </AlertDialog>
    );
}
