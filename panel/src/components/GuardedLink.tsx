"use client";

import React, { useState } from 'react';
import Link, { LinkProps } from 'next/link';
import { useRouter, usePathname } from 'next/navigation';
import {
    useUnsavedChangesState,
    UnsavedDialog,
} from '@/components/settings/UnsavedChanges';
import { leavesPage } from '@/lib/navGuard';

// GuardedLink behaves like next/link's <Link>, except that when any form on
// the current page has registered itself as dirty with the shared
// UnsavedChanges context, clicking it pops the Save / Discard / Cancel
// dialog instead of navigating immediately.
//
// When nothing is dirty (the common case) this is a transparent passthrough
// — no overhead, no behavior change. Wherever the app would normally use
// <Link>, swapping in <GuardedLink> is safe.

interface GuardedLinkProps extends Omit<LinkProps, 'onClick'> {
    children: React.ReactNode;
    className?: string;
    title?: string;
    target?: string;
    rel?: string;
    'aria-label'?: string;
    'aria-current'?: 'page' | undefined;
}

/**
 * The guard on its own, for links that cannot be a GuardedLink (a menu item
 * renders its own element and the menu unmounts on select, which would take
 * the dialog with it). Call `intercept` from the link's onClick and render
 * `dialog` somewhere that stays mounted:
 *   const nav = useGuardedNav();
 *   <Link href={h} onClick={e => nav.intercept(e, h)} />  ...  {nav.dialog}
 */
export function useGuardedNav(replace?: boolean) {
    const registration = useUnsavedChangesState();
    const router = useRouter();
    const pathname = usePathname();
    const [pending, setPending] = useState<string | null>(null);
    const [saving, setSaving] = useState(false);

    const intercept = (e: React.MouseEvent, hrefStr: string) => {
        // Let the user open in a new tab / window — that's a fresh page
        // load, beforeunload covers it.
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        if (!registration?.dirty) return;
        // Clicking the page you are already on is a no-op and skips the guard.
        // Anything else unmounts what is showing - see leavesPage for why the
        // ancestor case used to be waved through and what that cost.
        if (!leavesPage(pathname, hrefStr)) return;
        e.preventDefault();
        setPending(hrefStr);
    };

    const navigate = (href: string) => {
        if (replace) router.replace(href);
        else router.push(href);
    };

    const close = () => {
        setPending(null);
        setSaving(false);
    };

    const handleSave = async () => {
        if (!registration || pending === null) return;
        setSaving(true);
        let ok = false;
        try {
            ok = await registration.save();
        } catch {
            ok = false;
        }
        setSaving(false);
        // Stay on the page unless the work is actually stored. The catch alone
        // was not enough: a refused save - a server error, or the operator
        // answering "no" to a confirmation - resolves normally, so this used to
        // navigate away from exactly the edits it was protecting.
        if (!ok) return;
        const href = pending;
        close();
        navigate(href);
    };

    const handleDiscard = () => {
        if (!registration || pending === null) return;
        registration.discard();
        const href = pending;
        close();
        navigate(href);
    };

    const dialog = pending !== null && registration ? (
        <UnsavedDialog
            onSave={handleSave}
            onDiscard={handleDiscard}
            onCancel={close}
            saving={saving}
        />
    ) : null;

    return { intercept, dialog };
}

export default function GuardedLink({ children, ...props }: GuardedLinkProps) {
    const { intercept, dialog } = useGuardedNav(props.replace);
    const hrefStr = typeof props.href === 'string' ? props.href : '';

    return (
        <>
            <Link {...props} onClick={e => intercept(e, hrefStr)}>
                {children}
            </Link>
            {dialog}
        </>
    );
}
