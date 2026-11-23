'use client';

import { useEffect, useState } from 'react';
import { CircleAlert, CircleCheck, Copy, X } from 'lucide-react';
import { Toast, ToastClose, ToastRegion, toast as sdkToast } from '@ui-sdk/react/toast';
import { toastDuration } from './toastTiming';

/**
 * One toast host for the whole panel.
 *
 * There were 31 of these before, each a local `useState` plus its own copy of
 * the markup, with five different shapes of state and dismiss timeouts ranging
 * from 2800ms to 4500ms. The same action reported differently depending on
 * which screen you triggered it from, and six autosaving controls reported
 * nothing at all because their file had no toast to begin with.
 *
 * Deliberately a module-level singleton driven by one <ToastRoot/>, the same
 * shape as confirmDialog(): a call site changes by an import instead of also
 * having to own state and render a node. Fails SILENT rather than closed - a
 * missing host costs a message, and refusing the action the message describes
 * would be the worse trade.
 *
 * Lifetime, pause on hover or focus, announcements, swipe and stacking come from
 * the UI SDK ToastRegion; this file only keeps the call signature and the card.
 */

export interface ToastOptions {
    /** false renders the error treatment. Defaults to true. */
    ok?: boolean;
    /** Requested lifetime before the global 35% reduction; legacy minimums still apply. */
    durationMs?: number;
}

/**
 * toast shows a message. The second argument is the ok flag rather than an
 * options object because that is the signature the ~31 call sites already used
 * (`showToast(msg, ok = true)`), so migrating them is a rename.
 */
export function toast(message: string, ok: boolean = true, opts: ToastOptions = {}): void {
    const isOk = opts.ok ?? ok;
    sdkToast(message, { type: isOk ? 'success' : 'error', duration: toastDuration(isOk, opts.durationMs) });
}

/** Mount once, above every screen that can report something. */
export function ToastRoot() {
    // Inside the Beam desktop app the shell puts its settings button on the
    // bottom edge of every page (beam/app/launcher.go), right where this
    // stack and the download card sit. Marking the document lets globals.css
    // lift everything anchored there above it. Mounted once on every screen,
    // which is why it lives here. The hostname, not window.go: the runtime
    // that creates window.go may still be loading.
    useEffect(() => {
        if (window.location.hostname === 'wails.localhost' || window.location.protocol === 'wails:') {
            document.documentElement.dataset.beam = '';
        }
    }, []);

    return (
        <ToastRegion className="toast-container" maxVisible={8} shortcut={null}>
            {(item) => <ToastCard message={String(item.title ?? '')} ok={item.type !== 'error'} duration={item.duration} />}
        </ToastRegion>
    );
}

function ToastCard({ message, ok, duration }: { message: string; ok: boolean; duration?: number }) {
    const [copied, setCopied] = useState(false);
    const [copyError, setCopyError] = useState(false);
    const copy = async () => {
        try {
            await navigator.clipboard.writeText(message);
            setCopied(true);
            setCopyError(false);
        } catch {
            setCopyError(true);
        }
    };

    return (
        <Toast className="toast toast-dismissable toast-copyable">
            <svg className="toast-timer" aria-hidden="true">
                <rect
                    x="1"
                    y="1"
                    width="calc(100% - 2px)"
                    height="calc(100% - 2px)"
                    rx="12"
                    pathLength="1"
                    style={{
                        animationDuration: `${duration ?? 0}ms`,
                        stroke: ok ? 'var(--success-light)' : 'var(--error-light)',
                    }}
                />
            </svg>
            <button
                type="button"
                onClick={copy}
                className="flex items-center gap-2.5 min-w-0 flex-1 text-left"
                title="Click to copy message"
            >
                <div className={`toast-bar ${ok ? 'bg-(--success-light)' : 'bg-(--error-light)'}`} />
                {ok ? (
                    <CircleCheck size={14} className="text-(--success-light) shrink-0" />
                ) : (
                    <CircleAlert size={14} className="text-(--error-light) shrink-0" />
                )}
                <span className="min-w-0">
                    <span className="text-sm text-(--base-09) block break-words">{message}</span>
                    <span className="text-[10px] text-(--base-06) block mt-1">
                        {copyError
                            ? 'Copy unavailable. Select and copy the message manually.'
                            : copied
                              ? 'Copied to clipboard'
                              : 'Click to copy'}{' '}
                        <Copy size={10} className="inline" />
                    </span>
                </span>
            </button>
            <ToastClose className="btn btn-ghost btn-sm shrink-0" aria-label="Dismiss message">
                <X size={13} />
            </ToastClose>
        </Toast>
    );
}
