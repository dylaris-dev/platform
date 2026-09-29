"use client";

// ModalPanel is the inner panel of a modal: the `modal-panel` box inside a
// `modal-overlay`. It replaces the plain <div> every modal in the panel used,
// which gave a screen reader nothing - no dialog role, no name, focus left
// wherever it was on the page behind, and Tab walking out of the dialog into
// controls a mouse user could not reach through the overlay. All but three
// modals were built that way.
//
// What it adds, and nothing else:
//   - role="dialog" (or "alertdialog") and aria-modal
//   - a name: the panel's own `.modal-title` (else its first heading), found
//     after mount, so no header had to change; `label` for a panel without one
//   - focus moves into the dialog on open (unless something inside already
//     took it, e.g. an autoFocus input) and back to where it was on close
//   - Tab and Shift+Tab stay inside the panel
//   - Escape calls onClose - only when given one. A modal that could not be
//     dismissed without its buttons before still cannot; this does not invent
//     a way out of a form someone is halfway through.
//
// The markup, classes and children are the caller's, unchanged.

import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { FOCUSABLE, nextTrapIndex, ownsKey } from './modalFocus';

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
    const panelRef = useRef<HTMLElement>(null);
    const [labelledBy, setLabelledBy] = useState<string | undefined>(undefined);

    // The newest handler without re-registering the listener on every render.
    const closeRef = useRef(onClose);
    const dismissibleRef = useRef(dismissible);
    useLayoutEffect(() => {
        closeRef.current = onClose;
        dismissibleRef.current = dismissible;
    });

    useLayoutEffect(() => {
        const panel = panelRef.current;
        if (!panel) return;
        // Panels built before the modal classes existed name themselves with a
        // plain heading instead.
        const title = panel.querySelector<HTMLElement>('.modal-title')
            ?? panel.querySelector<HTMLElement>('h1, h2, h3, h4');
        if (title) {
            if (!title.id) title.id = `modal-title-${++titleSeq}`;
            setLabelledBy(title.id);
        }
    }, []);

    useEffect(() => {
        const panel = panelRef.current;
        if (!panel) return;
        const opener = document.activeElement as HTMLElement | null;
        if (!panel.contains(document.activeElement)) panel.focus();

        const onKey = (e: KeyboardEvent) => {
            const active = document.activeElement;
            if (!ownsKey(panel.contains(active), active === document.body || active === null)) return;
            if (e.key === 'Escape') {
                if (closeRef.current && dismissibleRef.current) {
                    e.preventDefault();
                    closeRef.current();
                }
                return;
            }
            if (e.key !== 'Tab') return;
            const items = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE))
                .filter(el => el.offsetParent !== null || el === document.activeElement);
            e.preventDefault();
            const next = nextTrapIndex(items.length, items.indexOf(active as HTMLElement), e.shiftKey);
            if (next >= 0) items[next].focus();
        };
        window.addEventListener('keydown', onKey);
        return () => {
            window.removeEventListener('keydown', onKey);
            // Back to the control that opened it, if it is still on the page.
            if (opener && opener !== document.body && document.contains(opener)) opener.focus();
        };
    }, []);

    return (
        <Tag
            {...rest}
            ref={panelRef as React.Ref<HTMLDivElement & HTMLFormElement>}
            role={role}
            aria-modal="true"
            aria-labelledby={labelledBy}
            aria-label={labelledBy ? undefined : label}
            tabIndex={-1}
            // Focus lands on the panel itself so a screen reader announces the
            // dialog; a ring around the whole box would only look like a bug.
            className={`${rest.className ?? ''} focus:outline-none`}
            onClick={e => { e.stopPropagation(); onClick?.(e); }}
        >
            {children}
        </Tag>
    );
}
