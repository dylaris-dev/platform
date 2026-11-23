"use client";

// ModalPanel is the inner panel of a modal: the `modal-panel` box inside a
// caller-owned `modal-overlay`. It is a UI SDK Dialog (an Alert Dialog for
// role="alertdialog"), which gives it the dialog role, aria-modal, focus moved
// in on open, Tab kept inside, Escape for the topmost layer and a name.
//
// The caller's markup stays in charge of layout:
//   - `inline`: the panel renders where it is, inside the caller's overlay,
//     instead of in the top layer, so the overlay keeps centring and dimming it
//   - the overlay's own onClick handles press-outside, so the SDK's
//     (closeOnOutsidePress) is off and the two cannot both fire
//   - the panel is open exactly while it is mounted
//   - Escape calls onClose - only when given one, and only while `dismissible`.
//     A modal that could not be dismissed without its buttons before still
//     cannot; this does not invent a way out of a form someone is halfway through.
//   - the name is the panel's own `.modal-title` (else its first heading), so no
//     header had to change; `label` names a panel without one
//
// It lives in this package rather than the panel so the file browser's own
// modals can use it; the panel re-exports it from components/ui/ModalPanel.

import React, { useEffect, useState } from 'react';
import { AlertDialog, AlertDialogContent } from '@ui-sdk/react/alert-dialog';
import { Dialog, DialogContent } from '@ui-sdk/react/dialog';

let titleSeq = 0;

type ModalPanelProps = React.HTMLAttributes<HTMLElement> & {
    // A panel that IS the form keeps being one, so Enter still submits it.
    as?: 'div' | 'form';
    onClose?: () => void;
    // Escape is ignored while false - for a dialog that is mid-request.
    dismissible?: boolean;
    role?: 'dialog' | 'alertdialog';
    // aria-label for a panel with no `.modal-title` to point at.
    label?: string;
};

export default function ModalPanel({
    as: Tag = 'div', onClose, dismissible = true, role = 'dialog', label, onClick, children, ...rest
}: ModalPanelProps) {
    const [labelledBy, setLabelledBy] = useState<string | undefined>(undefined);
    // Read during the first render, before focus moves into the panel.
    const [opener] = useState(() => (typeof document === 'undefined' ? null : document.activeElement as HTMLElement | null));

    // The SDK only restores focus on its own close; the caller unmounts us instead.
    useEffect(() => () => {
        if (opener && opener !== document.body && document.contains(opener)) opener.focus();
    }, [opener]);

    const alert = role === 'alertdialog';
    const Root = (alert ? AlertDialog : Dialog) as typeof Dialog;
    const Content = (alert ? AlertDialogContent : DialogContent) as typeof DialogContent;

    const nameFrom = (panel: HTMLElement | null) => {
        if (!panel) return;
        // Panels built before the modal classes existed name themselves with a
        // plain heading instead.
        const title = panel.querySelector<HTMLElement>('.modal-title')
            ?? panel.querySelector<HTMLElement>('h1, h2, h3, h4');
        if (title) {
            if (!title.id) title.id = `modal-title-${++titleSeq}`;
            setLabelledBy(title.id);
        }
    };

    return (
        <Root
            open
            modal={alert ? undefined : 'trap-focus'}
            closeOnOutsidePress={false}
            dismissible={dismissible && !!onClose}
            onOpenChange={open => { if (!open) onClose?.(); }}
        >
            <Content
                {...(rest as React.ComponentProps<typeof DialogContent>)}
                inline
                aria-modal="true"
                ref={nameFrom as React.Ref<HTMLDivElement>}
                render={Tag === 'form' ? <form /> : undefined}
                aria-labelledby={labelledBy}
                aria-label={labelledBy ? undefined : label}
                // Focus lands inside the panel; a ring around the whole box would only look like a bug.
                className={`${rest.className ?? ''} focus:outline-none`}
                onClick={(e: React.MouseEvent<HTMLDivElement>) => { e.stopPropagation(); onClick?.(e); }}
            >
                {children}
            </Content>
        </Root>
    );
}
